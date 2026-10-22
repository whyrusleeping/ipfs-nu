// Command quicperf measures plain quic-go throughput between two hosts: one connection,
// one stream, no Nu (docs/validation.md "LAN benchmarks"; Rust counterpart in
// experiments/quicperf-rs). Set QUIC_GO_DISABLE_GSO=true on the sender to compare.
//
//	quicperf -listen 0.0.0.0:4999
//	quicperf -dial 192.168.1.200:4999 -bytes 4294967296
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"math/big"
	"time"

	quic "github.com/quic-go/quic-go"
)

func main() {
	listen := flag.String("listen", "", "server address")
	dial := flag.String("dial", "", "client: server address")
	size := flag.Int64("bytes", 4<<30, "bytes to send")
	flag.Parse()
	if *listen != "" {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(24 * time.Hour)}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		tc := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"quicperf"}}
		ln, err := quic.ListenAddr(*listen, tc, nil)
		if err != nil {
			panic(err)
		}
		for {
			c, err := ln.Accept(context.Background())
			if err != nil {
				panic(err)
			}
			go func() {
				for {
					s, err := c.AcceptStream(context.Background())
					if err != nil {
						return
					}
					go func() { io.Copy(io.Discard, s); s.Write([]byte("k")); s.Close() }()
				}
			}()
		}
	}
	c, err := quic.DialAddr(context.Background(), *dial, &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"quicperf"}}, nil)
	if err != nil {
		panic(err)
	}
	buf := make([]byte, 64<<10)
	start := time.Now()
	s, err := c.OpenStreamSync(context.Background())
	if err != nil {
		panic(err)
	}
	var sent int64
	for sent < *size {
		if _, err := s.Write(buf); err != nil {
			panic(err)
		}
		sent += int64(len(buf))
	}
	s.Close()
	io.ReadAll(s) // the server's ack after the last byte
	d := time.Since(start)
	fmt.Printf("quic-go: %.0f MB/s (%.1f s)\n", float64(sent)/1e6/d.Seconds(), d.Seconds())
	c.CloseWithError(0, "done")
}
