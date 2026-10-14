package ipam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

var (
	ErrInvalidPool      = errors.New("invalid IPv4 pool")
	ErrPoolExhausted    = errors.New("IPv4 pool exhausted")
	ErrDuplicateSession = errors.New("session already exist")
	ErrSessionNotFound  = errors.New("unable to find session")
	ErrInvalidSession   = errors.New("invalid session")
	ErrAlreadyReleased  = errors.New("ip for session already release")
)

type Pool struct {
	mu        sync.Mutex
	prefix    netip.Prefix
	broadcast netip.Addr
	allocated map[netip.Addr]string
	reserved  map[netip.Addr]struct{}
	bySession map[string]netip.Addr
}

func NewPool(cidr string) (*Pool, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPool, err)
	}
	prefix = prefix.Masked()

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
		bySession: make(map[string]netip.Addr),
	}, nil
}

func (p *Pool) Allocate(sessionID string) (netip.Addr, error) {
	if sessionID == "" {
		return netip.Addr{}, ErrInvalidSession
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	//return the ip when sessionid already exist no error just ip return
	if ipAddr, exist := p.bySession[sessionID]; exist {
		return ipAddr, nil
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
		p.bySession[sessionID] = ip
		return ip, nil
	}

	return netip.Addr{}, ErrPoolExhausted
}

func (p *Pool) Release(sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sessionID == "" {
		return ErrInvalidSession
	}

	ip, ok := p.bySession[sessionID]
	if !ok {
		return ErrAlreadyReleased
	}
	delete(p.bySession, sessionID)
	delete(p.allocated, ip)
	return nil

}

type IpmStat struct {
	Total, Available, Used int
}

func (p *Pool) Stats() IpmStat {
	p.mu.Lock()
	defer p.mu.Unlock()

	hostBit := 32 - p.prefix.Bits()
	total := (1 << hostBit) - 2 //.0 and .255
	if total < 0 {
		total = 0
	}
	used := len(p.allocated)
	return IpmStat{Total: total, Available: total - used, Used: used}
}
