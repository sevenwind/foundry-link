package main

import (
	"encoding/base32"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"net/netip"
	"strings"
	"unicode/utf8"
)

// Код (комнаты или игрока) = версия, роль, флаги, тип NAT, отпечаток сертификата,
// IPv4:порт и/или IPv6:порт, имя (только у игрока), контрольная сумма.
// Он же служит каналом доверия: отпечаток закрепляется на обеих сторонах.

const (
	roleHost   byte = 1
	rolePlayer byte = 2

	flagV4   = 1
	flagV6   = 2
	flagName = 4

	codePrefix = "FL1-"
	maxNameLen = 24
)

type Code struct {
	Role byte
	NAT  natKind
	FP   [16]byte
	V4   netip.AddrPort
	V6   netip.AddrPort
	Name string
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func (c Code) Encode() string {
	var flags byte
	if c.V4.IsValid() {
		flags |= flagV4
	}
	if c.V6.IsValid() {
		flags |= flagV6
	}
	name := c.Name
	for len(name) > maxNameLen || !utf8.ValidString(name) {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name != "" {
		flags |= flagName
	}
	buf := []byte{1, c.Role, flags, byte(c.NAT)}
	buf = append(buf, c.FP[:]...)
	if c.V4.IsValid() {
		a := c.V4.Addr().As4()
		buf = append(buf, a[:]...)
		buf = binary.BigEndian.AppendUint16(buf, c.V4.Port())
	}
	if c.V6.IsValid() {
		a := c.V6.Addr().As16()
		buf = append(buf, a[:]...)
		buf = binary.BigEndian.AppendUint16(buf, c.V6.Port())
	}
	if name != "" {
		buf = append(buf, byte(len(name)))
		buf = append(buf, name...)
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(crc32.ChecksumIEEE(buf)))

	s := b32.EncodeToString(buf)
	var parts []string
	for len(s) > 5 {
		parts = append(parts, s[:5])
		s = s[5:]
	}
	parts = append(parts, s)
	return codePrefix + strings.Join(parts, "-")
}

func DecodeCode(s string) (Code, error) {
	var c Code
	s = strings.ToUpper(s)
	s = strings.NewReplacer(" ", "", "-", "", "\t", "", "\r", "", "\n", "").Replace(s)
	if !strings.HasPrefix(s, "FL1") {
		return c, errors.New("код должен начинаться с FL1-")
	}
	raw, err := b32.DecodeString(strings.TrimPrefix(s, "FL1"))
	if err != nil || len(raw) < 4+16+2 {
		return c, errors.New("код повреждён или скопирован не целиком")
	}
	body, sum := raw[:len(raw)-2], binary.BigEndian.Uint16(raw[len(raw)-2:])
	if uint16(crc32.ChecksumIEEE(body)) != sum {
		return c, errors.New("контрольная сумма не сходится: в коде опечатка или он скопирован не целиком")
	}
	if body[0] != 1 {
		return c, errors.New("код от другой версии программы")
	}
	c.Role, c.NAT = body[1], natKind(body[3])
	flags := body[2]
	copy(c.FP[:], body[4:20])
	p := body[20:]
	bad := errors.New("код повреждён")
	if flags&flagV4 != 0 {
		if len(p) < 6 {
			return c, bad
		}
		c.V4 = netip.AddrPortFrom(netip.AddrFrom4([4]byte(p[:4])), binary.BigEndian.Uint16(p[4:6]))
		p = p[6:]
	}
	if flags&flagV6 != 0 {
		if len(p) < 18 {
			return c, bad
		}
		c.V6 = netip.AddrPortFrom(netip.AddrFrom16([16]byte(p[:16])), binary.BigEndian.Uint16(p[16:18]))
		p = p[18:]
	}
	if flags&flagName != 0 {
		if len(p) < 1 || len(p) < 1+int(p[0]) {
			return c, bad
		}
		c.Name = string(p[1 : 1+int(p[0])])
		p = p[1+int(p[0]):]
	}
	if len(p) != 0 || (!c.V4.IsValid() && !c.V6.IsValid()) {
		return c, bad
	}
	if c.Role != roleHost && c.Role != rolePlayer {
		return c, bad
	}
	return c, nil
}
