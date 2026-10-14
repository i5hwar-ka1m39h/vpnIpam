package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"net/netip"
	"sync"
)

var (
	ErrInvalidPool      = errors.New("Invalid IPv4 pool")
	ErrPoolExhausted    = errors.New("IPv4 pool exhausted")
	ErrDuplicateSession = errors.New("Session already exist")
	ErrSessionNotFound  = errors.New("Unable to find session")
)

type Pool struct {
	mu        sync.Mutex
	prefix    netip.Prefix
	broadcast netip.Addr
	allocated map[netip.Addr]string
	reserved  map[netip.Addr]struct{}
}

func NewPool(cidr string) (*Pool, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPool, err)
	}

	if !prefix.Addr().Is4() || prefix.Bits() < 8 || prefix.Bits() > 32 {
		return nil, ErrInvalidPool
	}

	addr := prefix.Addr().As4()
	base := binary.BigEndian.Uint32(addr[:])
	hostbit := uint(32 - prefix.Bits())

	broadcast_value := base | (uint32(1)<<uint32(hostbit) - 1)

	var broadcastBytes [4]byte

	binary.BigEndian.PutUint32(broadcastBytes[:], broadcast_value)

	return &Pool{
		prefix:    prefix,
		broadcast: netip.AddrFrom4(broadcastBytes),
		allocated: make(map[netip.Addr]string),
		reserved: map[netip.Addr]struct{}{
			prefix.Addr():                   {},
			netip.AddrFrom4(broadcastBytes): {},
		},
	}, nil
}

func (p *Pool) Allocate(sessionID string) (netip.Addr, error) {
	if sessionID == "" {
		return netip.Addr{}, errors.New("a session id is required")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	for ip, owner := range p.allocated {
		if owner == sessionID {
			return ip, ErrDuplicateSession
		}

	}

	for ip := p.prefix.Addr().Next(); p.prefix.Contains(ip); ip = ip.Next() {
		if ip == p.broadcast {
			break
		}

		if _, ok := p.reserved[ip]; ok {
			continue
		}

		if _, ok := p.allocated[ip]; ok {
			continue
		}

		p.allocated[ip] = sessionID
		return ip, nil
	}

	return netip.Addr{}, ErrPoolExhausted
}
