// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/netip"
	"syscall"
	"time"

	"github.com/mdlayher/socket"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
	"tailscale.com/net/stun"
)

const (
	timestampingFlags = unix.SOF_TIMESTAMPING_TX_SOFTWARE | // tx timestamp generation in device driver
		unix.SOF_TIMESTAMPING_RX_SOFTWARE | // rx timestamp generation in the kernel
		unix.SOF_TIMESTAMPING_SOFTWARE // report software timestamps
)

func getUDPConnKernelTimestamp() (io.ReadWriteCloser, error) {
	sconn, err := socket.Socket(unix.AF_INET6, unix.SOCK_DGRAM, unix.IPPROTO_UDP, "udp", nil)
	if err != nil {
		return nil, err
	}
	sa := unix.SockaddrInet6{}
	err = sconn.Bind(&sa)
	if err != nil {
		return nil, err
	}
	err = sconn.SetsockoptInt(unix.SOL_SOCKET, unix.SO_TIMESTAMPING_NEW, timestampingFlags)
	if err != nil {
		return nil, err
	}
	return sconn, nil
}

func parseTimestampFromCmsgs(oob []byte) (time.Time, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}, fmt.Errorf("error parsing oob as cmsgs: %w", err)
	}
	for _, msg := range msgs {
		if msg.Header.Level == unix.SOL_SOCKET && msg.Header.Type == unix.SO_TIMESTAMPING_NEW && len(msg.Data) >= 16 {
			sec := int64(binary.NativeEndian.Uint64(msg.Data[:8]))
			ns := int64(binary.NativeEndian.Uint64(msg.Data[8:16]))
			return time.Unix(sec, ns), nil
		}
	}
	return time.Time{}, errors.New("failed to parse timestamp from cmsgs")
}

func mkICMPMeasureFn(source timestampSource) measureFn {
	return func(conn io.ReadWriteCloser, hostname string, dst netip.AddrPort) (rtt time.Duration, err error) {
		return measureICMPRTT(source, conn, hostname, dst)
	}
}

func measureICMPRTT(source timestampSource, conn io.ReadWriteCloser, _ string, dst netip.AddrPort) (rtt time.Duration, err error) {
	sconn, ok := conn.(*socket.Conn)
	if !ok {
		return 0, fmt.Errorf("conn of unexpected type: %T", conn)
	}
	txBody := &icmp.Echo{
		// The kernel overrides this and routes appropriately so there is no
		// point in setting or verifying.
		ID: 0,
		// Make this sufficiently random so that we do not account a late
		// arriving reply in a future probe window.
		Seq: int(rand.Int32N(math.MaxUint16)),
		// Fingerprint ourselves.
		Data: []byte("stunstamp"),
	}
	txMsg := icmp.Message{
		Body: txBody,
	}
	var to unix.Sockaddr
	if dst.Addr().Is4() {
		txMsg.Type = ipv4.ICMPTypeEcho
		to = &unix.SockaddrInet4{}
		copy(to.(*unix.SockaddrInet4).Addr[:], dst.Addr().AsSlice())
	} else {
		txMsg.Type = ipv6.ICMPTypeEchoRequest
		to = &unix.SockaddrInet6{}
		copy(to.(*unix.SockaddrInet6).Addr[:], dst.Addr().AsSlice())
	}
	txBuf, err := txMsg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	txAt := time.Now()
	err = sconn.Sendto(context.Background(), txBuf, 0, to)
	if err != nil {
		return 0, fmt.Errorf("sendto error: %v", err)
	}

	if source == timestampSourceKernel {
		txCtx, txCancel := context.WithTimeout(context.Background(), txRxTimeout)
		defer txCancel()

		buf := make([]byte, 1024)
		oob := make([]byte, 1024)

		for {
			n, oobn, _, _, err := sconn.Recvmsg(txCtx, buf, oob, unix.MSG_ERRQUEUE)
			if err != nil {
				return 0, fmt.Errorf("recvmsg (MSG_ERRQUEUE) error: %v", err) // don't wrap
			}

			buf = buf[:n]
			// Spin until we find the message we sent. We get the full packet
			// looped including eth header so match against the tail.
			if n < len(txBuf) {
				continue
			}
			txLoopedMsg, err := icmp.ParseMessage(txMsg.Type.Protocol(), buf[len(buf)-len(txBuf):])
			if err != nil {
				continue
			}
			txLoopedBody, ok := txLoopedMsg.Body.(*icmp.Echo)
			if !ok || txLoopedBody.Seq != txBody.Seq || txLoopedMsg.Code != txMsg.Code ||
				txLoopedMsg.Type != txLoopedMsg.Type || !bytes.Equal(txLoopedBody.Data, txBody.Data) {
				continue
			}
			txAt, err = parseTimestampFromCmsgs(oob[:oobn])
			if err != nil {
				return 0, fmt.Errorf("failed to get tx timestamp: %v", err) // don't wrap
			}
			break
		}
	}

	rxCtx, rxCancel := context.WithTimeout(context.Background(), txRxTimeout)
	defer rxCancel()

	rxBuf := make([]byte, 1024)
	oob := make([]byte, 1024)
	for {
		n, oobn, _, _, err := sconn.Recvmsg(rxCtx, rxBuf, oob, 0)
		if err != nil {
			return 0, fmt.Errorf("recvmsg error: %w", err)
		}
		rxAt := time.Now()
		rxMsg, err := icmp.ParseMessage(txMsg.Type.Protocol(), rxBuf[:n])
		if err != nil {
			continue
		}
		if txMsg.Type == ipv4.ICMPTypeEcho {
			if rxMsg.Type != ipv4.ICMPTypeEchoReply {
				continue
			}
		} else {
			if rxMsg.Type != ipv6.ICMPTypeEchoReply {
				continue
			}
		}
		if rxMsg.Code != txMsg.Code {
			continue
		}
		rxBody, ok := rxMsg.Body.(*icmp.Echo)
		if !ok || rxBody.Seq != txBody.Seq || !bytes.Equal(rxBody.Data, txBody.Data) {
			continue
		}
		if source == timestampSourceKernel {
			rxAt, err = parseTimestampFromCmsgs(oob[:oobn])
			if err != nil {
				return 0, fmt.Errorf("failed to get rx timestamp: %v", err)
			}
		}
		return rxAt.Sub(txAt), nil
	}
}

func measureSTUNRTTKernel(conn io.ReadWriteCloser, _ string, dst netip.AddrPort) (rtt time.Duration, err error) {
	sconn, ok := conn.(*socket.Conn)
	if !ok {
		return 0, fmt.Errorf("conn of unexpected type: %T", conn)
	}

	var to unix.Sockaddr
	if dst.Addr().Is4() {
		to = &unix.SockaddrInet4{
			Port: int(dst.Port()),
		}
		copy(to.(*unix.SockaddrInet4).Addr[:], dst.Addr().AsSlice())
	} else {
		to = &unix.SockaddrInet6{
			Port: int(dst.Port()),
		}
		copy(to.(*unix.SockaddrInet6).Addr[:], dst.Addr().AsSlice())
	}

	txID := stun.NewTxID()
	req := stun.Request(txID)

	err = sconn.Sendto(context.Background(), req, 0, to)
	if err != nil {
		return 0, fmt.Errorf("sendto error: %v", err) // don't wrap
	}

	txCtx, txCancel := context.WithTimeout(context.Background(), txRxTimeout)
	defer txCancel()

	buf := make([]byte, 1024)
	oob := make([]byte, 1024)
	var txAt time.Time

	for {
		n, oobn, _, _, err := sconn.Recvmsg(txCtx, buf, oob, unix.MSG_ERRQUEUE)
		if err != nil {
			return 0, fmt.Errorf("recvmsg (MSG_ERRQUEUE) error: %v", err) // don't wrap
		}

		buf = buf[:n]
		if n < len(req) || !bytes.Equal(req, buf[len(buf)-len(req):]) {
			// Spin until we find the message we sent. We get the full packet
			// looped including eth header so match against the tail.
			continue
		}
		txAt, err = parseTimestampFromCmsgs(oob[:oobn])
		if err != nil {
			return 0, fmt.Errorf("failed to get tx timestamp: %v", err) // don't wrap
		}
		break
	}

	rxCtx, rxCancel := context.WithTimeout(context.Background(), txRxTimeout)
	defer rxCancel()

	for {
		n, oobn, _, _, err := sconn.Recvmsg(rxCtx, buf, oob, 0)
		if err != nil {
			return 0, fmt.Errorf("recvmsg error: %w", err) // wrap for timeout-related error unwrapping
		}

		gotTxID, _, err := stun.ParseResponse(buf[:n])
		if err != nil || gotTxID != txID {
			// Spin until we find the txID we sent. We may end up reading
			// extremely late arriving responses from previous intervals. As
			// such, we can't be certain if we're parsing the "current"
			// response, so spin for parse errors too.
			continue
		}

		rxAt, err := parseTimestampFromCmsgs(oob[:oobn])
		if err != nil {
			return 0, fmt.Errorf("failed to get rx timestamp: %v", err) // don't wrap
		}

		return rxAt.Sub(txAt), nil
	}

}

func getICMPConn(forDst netip.Addr, source timestampSource) (io.ReadWriteCloser, error) {
	domain := unix.AF_INET
	proto := unix.IPPROTO_ICMP
	if forDst.Is6() {
		domain = unix.AF_INET6
		proto = unix.IPPROTO_ICMPV6
	}
	conn, err := socket.Socket(domain, unix.SOCK_DGRAM, proto, "icmp", nil)
	if err != nil {
		return nil, err
	}
	if source == timestampSourceKernel {
		err = conn.SetsockoptInt(unix.SOL_SOCKET, unix.SO_TIMESTAMPING_NEW, timestampingFlags)
	}
	return conn, err
}

func getProtocolSupportInfo(p protocol) protocolSupportInfo {
	switch p {
	case protocolSTUN:
		return protocolSupportInfo{
			kernelTS:    true,
			userspaceTS: true,
			stableConn:  true,
		}
	case protocolHTTPS:
		return protocolSupportInfo{
			kernelTS:    false,
			userspaceTS: true,
			stableConn:  true,
		}
	case protocolTCP:
		return protocolSupportInfo{
			kernelTS:    true,
			userspaceTS: false,
			stableConn:  true,
		}
	case protocolICMP:
		return protocolSupportInfo{
			kernelTS:    true,
			userspaceTS: true,
			stableConn:  false,
		}
	}
	return protocolSupportInfo{}
}

func setSOReuseAddr(fd uintptr) error {
	// we may restart faster than TIME_WAIT can clear
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
}
