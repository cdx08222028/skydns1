// Copyright (c) 2013 Erik St. Martin, Brian Ketelsen. All rights reserved.
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package server

import (
	"crypto/sha1"
	"github.com/miekg/dns"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const origTTL uint32 = 60

var cache *sigCache = newCache()
var inflight *single = new(single)

// ParseKeyFile read a DNSSEC keyfile as generated by dnssec-keygen or other
// utilities. It add ".key" for the public key and ".private" for the private key.
func ParseKeyFile(file string) (*dns.DNSKEY, dns.PrivateKey, error) {
	f, e := os.Open(file + ".key")
	if e != nil {
		return nil, nil, e
	}
	k, e := dns.ReadRR(f, file+".key")
	if e != nil {
		return nil, nil, e
	}
	f, e = os.Open(file + ".private")
	if e != nil {
		return nil, nil, e
	}
	p, e := k.(*dns.DNSKEY).ReadPrivateKey(f, file+".private")
	if e != nil {
		return nil, nil, e
	}
	k.Header().Ttl = origTTL
	return k.(*dns.DNSKEY), p, nil
}

// nsec creates (if needed) NSEC records that are included in the reply.
func (s *Server) nsec(m *dns.Msg) {
	if m.Rcode == dns.RcodeNameError {
		// qname nsec
		nsec1 := s.newNSEC(m.Question[0].Name)
		m.Ns = append(m.Ns, nsec1)
		// wildcard nsec
		idx := dns.Split(m.Question[0].Name)
		wildcard := "*." + m.Question[0].Name[idx[0]:]
		nsec2 := s.newNSEC(wildcard)
		if nsec1.Hdr.Name != nsec2.Hdr.Name || nsec1.NextDomain != nsec2.NextDomain {
			// different NSEC, add it
			m.Ns = append(m.Ns, nsec2)
		}
	}
	if m.Rcode == dns.RcodeSuccess && len(m.Ns) == 1 {
		if _, ok := m.Ns[0].(*dns.SOA); ok {
			m.Ns = append(m.Ns, s.newNSEC(m.Question[0].Name))
		}
	}
}

// sign signs a message m, it takes care of negative or nodata responses as
// well by synthesising NSEC records. It will also cache the signatures, using
// a hash of the signed data as a key.
// We also fake the origin TTL in the signature, because we don't want to
// throw away signatures when services decide to have longer TTL. So we just
// set the origTTL to 60.
func (s *Server) sign(m *dns.Msg, bufsize uint16) {
	now := time.Now().UTC()
	incep := uint32(now.Add(-2 * time.Hour).Unix())     // 2 hours, be sure to catch daylight saving time and such
	expir := uint32(now.Add(7 * 24 * time.Hour).Unix()) // sign for a week

	// TODO(miek): repeating this two times?
	for _, r := range rrSets(m.Answer) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		key := cache.key(r)
		if s := cache.search(key); s != nil {
			if s.ValidityPeriod(now.Add(-24 * time.Hour)) {
				m.Answer = append(m.Answer, s)
				continue
			}
			cache.remove(key)
		}
		sig, err, shared := inflight.Do(key, func() (*dns.RRSIG, error) {
			sig1 := s.newRRSIG(incep, expir)
			e := sig1.Sign(s.Privkey, r)
			if e != nil {
				log.Printf("Failed to sign: %s\n", e.Error())
			}
			return sig1, e
		})
		if err != nil {
			continue
		}
		if !shared {
			// is it possible to miss this, due the the c.dups > 0 in Do()? TODO(miek)
			cache.insert(key, sig)
		}
		m.Answer = append(m.Answer, dns.Copy(sig).(*dns.RRSIG))
	}
	for _, r := range rrSets(m.Ns) {
		if r[0].Header().Rrtype == dns.TypeRRSIG {
			continue
		}
		key := cache.key(r)
		if s := cache.search(key); s != nil {
			if s.ValidityPeriod(now.Add(-24 * time.Hour)) {
				m.Ns = append(m.Ns, s)
				continue
			}
			cache.remove(key)
		}
		sig, err, shared := inflight.Do(key, func() (*dns.RRSIG, error) {
			sig1 := s.newRRSIG(incep, expir)
			e := sig1.Sign(s.Privkey, r)
			if e != nil {
				log.Printf("Failed to sign: %s\n", e.Error())
			}
			return sig1, e
		})
		if err != nil {
			continue
		}
		if !shared {
			// is it possible to miss this, due the the c.dups > 0 in Do()? TODO(miek)
			cache.insert(key, sig)
		}
		m.Ns = append(m.Ns, dns.Copy(sig).(*dns.RRSIG))
	}
	// TODO(miek): Forget the additional section for now
	if bufsize >= 512 || bufsize <= 4096 {
		m.Truncated = m.Len() > int(bufsize)
	}
	o := new(dns.OPT)
	o.Hdr.Name = "."
	o.Hdr.Rrtype = dns.TypeOPT
	o.SetDo()
	o.SetUDPSize(4096)
	m.Extra = append(m.Extra, o)
	return
}

func (s *Server) newRRSIG(incep, expir uint32) *dns.RRSIG {
	sig := new(dns.RRSIG)
	sig.Hdr.Rrtype = dns.TypeRRSIG
	sig.Hdr.Ttl = origTTL
	sig.OrigTtl = origTTL
	sig.Algorithm = s.Dnskey.Algorithm
	sig.KeyTag = s.KeyTag
	sig.Inception = incep
	sig.Expiration = expir
	sig.SignerName = s.Dnskey.Hdr.Name
	return sig
}

// newNSEC returns the NSEC record need to denial qname, or gives back a NODATA NSEC.
func (s *Server) newNSEC(qname string) *dns.NSEC {
	qlabels := dns.SplitDomainName(qname)
	if len(qlabels) < s.domainLabels {
		// TODO(miek): can not happen...?
	}
	// Strip the last s.domainLabels, return up to 4 before
	// that. Four labels is the maximum qname we can handle.
	ls := len(qlabels) - s.domainLabels
	ls4 := ls - 4
	if ls4 < 0 {
		ls4 = 0
	}
	key := qlabels[ls4:ls]
	prev, next := s.registry.GetNSEC(strings.Join(key, "."))
	nsec := &dns.NSEC{Hdr: dns.RR_Header{Name: prev + s.domain + ".", Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: 60},
		NextDomain: next + s.domain + "."}
	if prev == "" {
		nsec.TypeBitMap = []uint16{dns.TypeA, dns.TypeSOA, dns.TypeNS, dns.TypeAAAA, dns.TypeRRSIG, dns.TypeNSEC, dns.TypeDNSKEY}
	} else {
		nsec.TypeBitMap = []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeSRV, dns.TypeRRSIG, dns.TypeNSEC}
	}
	return nsec
}

type rrset struct {
	qname string
	qtype uint16
}

func rrSets(rrs []dns.RR) map[rrset][]dns.RR {
	m := make(map[rrset][]dns.RR)
	for _, r := range rrs {
		if s, ok := m[rrset{r.Header().Name, r.Header().Rrtype}]; ok {
			s = append(s, r)
			m[rrset{r.Header().Name, r.Header().Rrtype}] = s
		} else {
			s := make([]dns.RR, 1, 3)
			s[0] = r
			m[rrset{r.Header().Name, r.Header().Rrtype}] = s
		}
	}
	if len(m) > 0 {
		return m
	}
	return nil
}

type sigCache struct {
	sync.RWMutex
	m map[string]*dns.RRSIG
}

func newCache() *sigCache {
	c := new(sigCache)
	c.m = make(map[string]*dns.RRSIG)
	return c
}

func (c *sigCache) remove(s string) {
	delete(c.m, s)
}

func (c *sigCache) insert(s string, r *dns.RRSIG) {
	c.Lock()
	defer c.Unlock()
	if _, ok := c.m[s]; !ok {
		c.m[s] = r
	}
}

func (c *sigCache) search(s string) *dns.RRSIG {
	c.RLock()
	defer c.RUnlock()
	if s, ok := c.m[s]; ok {
		// we want to return a copy here, because if we didn't the RRSIG
		// could be removed by another goroutine before the packet containing
		// this signature is send out.
		log.Println("DNS Signature retrieved from cache")
		return dns.Copy(s).(*dns.RRSIG)
	}
	return nil
}

// key uses the name, type and rdata, which is serialized and then hashed as the
// key for the lookup
func (c *sigCache) key(rrs []dns.RR) string {
	h := sha1.New()
	i := []byte(rrs[0].Header().Name)
	i = append(i, packUint16(rrs[0].Header().Rrtype)...)
	for _, r := range rrs {
		switch t := r.(type) { // we only do a few type, serialize these manually
		case *dns.SOA:
			i = append(i, packUint32(t.Serial)...)
			// we only fiddle with the serial so store that
		case *dns.SRV:
			i = append(i, packUint16(t.Priority)...)
			i = append(i, packUint16(t.Weight)...)
			i = append(i, packUint16(t.Weight)...)
			i = append(i, []byte(t.Target)...)
		case *dns.A:
			i = append(i, []byte(t.A)...)
		case *dns.AAAA:
			i = append(i, []byte(t.AAAA)...)
		case *dns.DNSKEY:
			// Need nothing more, the rdata stays the same during a run
		case *dns.NSEC:
			i = append(i, []byte(t.NextDomain)...)
			// bitmap does not differentiate
		default:
			log.Printf("DNS Signature for unhandled type %T seen", t)
		}
	}
	return string(h.Sum(i))
}

// TODO(miek): prolly should use the stdlib ones
func packUint16(i uint16) []byte { return []byte{byte(i >> 8), byte(i)} }
func packUint32(i uint32) []byte { return []byte{byte(i >> 24), byte(i >> 16), byte(i >> 8), byte(i)} }

// Adapted from singleinflight.go from the original Go Code. Copyright 2013 The Go Authors.
type call struct {
	wg   sync.WaitGroup
	val  *dns.RRSIG
	err  error
	dups int
}

type single struct {
	sync.Mutex
	m map[string]*call
}

func (g *single) Do(key string, fn func() (*dns.RRSIG, error)) (*dns.RRSIG, error, bool) {
	g.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dups++
		g.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(call)
	c.wg.Add(1)
	g.m[key] = c
	g.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.Lock()
	delete(g.m, key)
	g.Unlock()

	return c.val, c.err, c.dups > 0
}
