package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jedisct1/dlog"
	"github.com/miekg/dns"
	"golang.org/x/crypto/ed25519"
)

type CertInfo struct {
	ServerPk           [32]byte
	SharedKey          [32]byte
	MagicQuery         [ClientMagicLen]byte
	CryptoConstruction CryptoConstruction
	ForwardSecurity    bool
}

func FetchCurrentDNSCryptCert(proxy *Proxy, serverName *string, proto string, pk ed25519.PublicKey, serverAddress string, providerName string, isNew bool, relays []*Endpoint) ([]*Endpoint, CertInfo, int, error) {
	if len(pk) != ed25519.PublicKeySize {
		return nil, CertInfo{}, 0, errors.New("Invalid public key length")
	}
	if !strings.HasSuffix(providerName, ".") {
		providerName = providerName + "."
	}
	if serverName == nil {
		serverName = &providerName
	}
	query := dns.Msg{}
	query.SetQuestion(providerName, dns.TypeTXT)
	if !strings.HasPrefix(providerName, "2.dnscrypt-cert.") {
		dlog.Warnf("[%v] uses a non-standard provider name ('%v' doesn't start with '2.dnscrypt-cert.')", *serverName, providerName)
	}
	var in *dns.Msg
	var rtt time.Duration
	var err error
	var workingSet []*Endpoint
	var relay_f bool
	if len(relays) > 0 {
		for i , relayAddr := range relays {
			in, rtt, err, relay_f = dnsExchange(proxy, proto, &query, serverAddress, relayAddr, serverName)
			if err != nil {
				dlog.Debug(err)
				continue
			}
			if !relay_f {
				workingSet = append(workingSet, relayAddr)
			} else {
				dlog.Noticef("relay [%d] failed for [%s]", i + 1, *serverName)
			}
		}
		if len(workingSet) < 1 {
			dlog.Noticef("all relays failed for [%s]", *serverName)
			return nil, CertInfo{}, 0, errors.New("all relays failed")
		}
	} else {
		in, rtt, err, _ = dnsExchange(proxy, proto, &query, serverAddress, nil, serverName)
	}
	if err != nil {
		dlog.Debug(err)
		return nil, CertInfo{}, 0, err
	}
	now := uint32(time.Now().Unix())
	certInfo := CertInfo{CryptoConstruction: UndefinedConstruction}
	highestSerial := uint32(0)
	var certCountStr string
	for _, answerRr := range in.Answer {
		var txt string
		if t, ok := answerRr.(*dns.TXT); !ok {
			dlog.Noticef("[%v] extra record of type [%v] found in certificate", *serverName, answerRr.Header().Rrtype)
			continue
		} else {
			txt = strings.Join(t.Txt, "")
		}
		binCert := packTxtString(txt)
		if len(binCert) < 124 {
			dlog.Warnf("certificate of [%v] is too short", *serverName)
			continue
		}
		if !bytes.Equal(binCert[:4], CertMagic[:4]) {
			dlog.Warnf("[%v] has invalid cert magic", *serverName)
			continue
		}
		cryptoConstruction := CryptoConstruction(0)
		esVersion := binary.BigEndian.Uint16(binCert[4:6])
		switch esVersion {
		case 0x0001:
			cryptoConstruction = XSalsa20Poly1305
		case 0x0002:
			cryptoConstruction = XChacha20Poly1305
		default:
			dlog.Noticef("[%v] has unsupported crypto construction", *serverName)
			continue
		}
		signature := binCert[8:72]
		signed := binCert[72:]
		if !ed25519.Verify(pk, signed, signature) {
			dlog.Warnf("[%v] Incorrect signature for provider name: [%v]", *serverName, providerName)
			continue
		}
		serial := binary.BigEndian.Uint32(binCert[112:116])
		tsBegin := binary.BigEndian.Uint32(binCert[116:120])
		tsEnd := binary.BigEndian.Uint32(binCert[120:124])
		if tsBegin >= tsEnd {
			dlog.Warnf("[%v] certificate ends before it starts (%v >= %v)", *serverName, tsBegin, tsEnd)
			continue
		}
		ttl := tsEnd - tsBegin
		if ttl > 86400*7 {
			dlog.Infof("[%v] the key validity period for this server is excessively long (%d days), significantly reducing reliability and forward security.", *serverName, ttl/86400)
			daysLeft := (tsEnd - now) / 86400
			if daysLeft < 1 {
				dlog.Criticalf("[%v] certificate will expire today -- Switch to a different resolver as soon as possible", *serverName)
			} else if daysLeft <= 7 {
				dlog.Warnf("[%v] certificate is about to expire -- if you don't manage this server, tell the server operator about it", *serverName)
			} else if daysLeft <= 30 {
				dlog.Infof("[%v] certificate will expire in %d days", *serverName, daysLeft)
			}
			certInfo.ForwardSecurity = false
		} else {
			certInfo.ForwardSecurity = true
		}
		if !proxy.certIgnoreTimestamp {
			if now > tsEnd || now < tsBegin {
				dlog.Debugf("certificate of [%v] is invalid at the current date (now: %v is not in [%v..%v])", *serverName, now, tsBegin, tsEnd)
				continue
			}
		}
		if serial < highestSerial {
			dlog.Debugf("[%v] superseded by a previous certificate", *serverName)
			continue
		}
		if serial == highestSerial {
			if cryptoConstruction < certInfo.CryptoConstruction {
				dlog.Debugf("[%v] keeping the previous, preferred crypto construction", *serverName)
				continue
			} else {
				dlog.Debugf("[%v] upgrading the construction from %v to %v", *serverName, certInfo.CryptoConstruction, cryptoConstruction)
			}
		}
		if cryptoConstruction != XChacha20Poly1305 && cryptoConstruction != XSalsa20Poly1305 {
			dlog.Noticef("[%v] Crypto construction %v not supported", *serverName, cryptoConstruction)
			continue
		}
		var serverPk [32]byte
		copy(serverPk[:], binCert[72:104])
		sharedKey := ComputeSharedKey(cryptoConstruction, &proxy.proxySecretKey, &serverPk, &providerName)
		certInfo.SharedKey = sharedKey
		highestSerial = serial
		certInfo.CryptoConstruction = cryptoConstruction
		copy(certInfo.ServerPk[:], serverPk[:])
		copy(certInfo.MagicQuery[:], binCert[104:112])
		if isNew {
			dlog.Noticef("[%s] OK (DNSCrypt V%d) - rtt: %dms%s", *serverName, esVersion, rtt.Nanoseconds()/1000000, certCountStr)
		} else {
			dlog.Infof("[%s] OK (DNSCrypt V%d) - rtt: %dms%s", *serverName, esVersion, rtt.Nanoseconds()/1000000, certCountStr)
		}
		certCountStr = " - additional certificate"
	}
	if certInfo.CryptoConstruction == UndefinedConstruction {
		return nil, certInfo, 0, errors.New("No useable certificate found")
	}
	return workingSet, certInfo, int(rtt.Nanoseconds() / 1000000), nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func dddToByte(s []byte) byte {
	return byte((s[0]-'0')*100 + (s[1]-'0')*10 + (s[2] - '0'))
}

func packTxtString(s string) []byte {
	bs := make([]byte, len(s))
	msg := make([]byte, 0)
	copy(bs, s)
	for i := 0; i < len(bs); i++ {
		if bs[i] == '\\' {
			i++
			if i == len(bs) {
				break
			}
			if i+2 < len(bs) && isDigit(bs[i]) && isDigit(bs[i+1]) && isDigit(bs[i+2]) {
				msg = append(msg, dddToByte(bs[i:]))
				i += 2
			} else if bs[i] == 't' {
				msg = append(msg, '\t')
			} else if bs[i] == 'r' {
				msg = append(msg, '\r')
			} else if bs[i] == 'n' {
				msg = append(msg, '\n')
			} else {
				msg = append(msg, bs[i])
			}
		} else {
			msg = append(msg, bs[i])
		}
	}
	return msg
}

func dnsExchange(proxy *Proxy, proto string, query *dns.Msg, serverAddress string, relayAddr *Endpoint, serverName *string) (*dns.Msg, time.Duration, error, bool) {
	relay_f := relayAddr == nil
	response, ttl, err := _dnsExchange(proxy, proto, query, serverAddress, relayAddr)
	if err != nil && relayAddr != nil {
		dlog.Debugf("failed to get a certificate for [%v] via relay [%v], retrying over a direct connection", *serverName, relayAddr.IP)
		relay_f = true
		response, ttl, err = _dnsExchange(proxy, proto, query, serverAddress, nil)
		if err == nil {
			dlog.Infof("direct certificate retrieval for [%v] succeeded", *serverName)
		}
	}
	return response, ttl, err, relay_f
}

func _dnsExchange(proxy *Proxy, proto string, query *dns.Msg, serverAddress string, relayAddr *Endpoint) (*dns.Msg, time.Duration, error) {
	var packet []byte
	var rtt time.Duration
	if proto == "udp" {
		qNameLen, padding := len(query.Question[0].Name), 0
		if qNameLen < 480 {
			padding = 480 - qNameLen
		}
		if padding > 0 {
			opt := new(dns.OPT)
			opt.Hdr.Name = "."
			ext := new(dns.EDNS0_PADDING)
			ext.Padding = make([]byte, padding)
			opt.Option = append(opt.Option, ext)
			query.Extra = []dns.RR{opt}
		}
	}
	binQuery, err := query.Pack()
	if err != nil {
		return nil, 0, err
	}
	ipAddr, err := ResolveEndpoint(serverAddress)
	if err != nil {
		return nil, 0, err
	}
	upstreamAddr := ipAddr
	if relayAddr != nil {
		proxy.prepareForRelay(ipAddr, &binQuery)
		upstreamAddr = relayAddr
	}
	now := time.Now()
	var pc net.Conn
	proxyDialer := proxy.xTransport.proxyDialer
	if proto == "udp" || proxyDialer == nil {
		pc, err = net.Dial(proto, upstreamAddr.String())
	} else {
		pc, err = (*proxyDialer).Dial("tcp", upstreamAddr.String())
	}
	if err != nil {
		return nil, 0, err
	}
	defer pc.Close()
	if err = pc.SetDeadline(time.Now().Add(proxy.timeout)); err != nil {
		return nil, 0, err
	}
	if proto == "tcp" {
		binQuery, err = PrefixWithSize(binQuery)
		if err != nil {
			return nil, 0, err
		}
	}
	if _, err = pc.Write(binQuery); err != nil {
		return nil, 0, err
	}
	if proto == "udp" {
		var length int
		packet = make([]byte, MaxDNSPacketSize)
		length, err = pc.Read(packet)
		packet = packet[:length]
	} else {
		packet, err = ReadPrefixed(&pc)
	}
	if err != nil {
		return nil, 0, err
	}
	rtt = time.Since(now)
	
	msg := dns.Msg{}
	if err := msg.Unpack(packet); err != nil {
		return nil, 0, err
	}
	return &msg, rtt, nil
}
