package effects

import (
	"fmt"
	"net"
	"syscall"
)

// UDPWaker sends Wake-on-LAN magic packets over a directed UDP broadcast.
// nut-dog runs on the host network, so the packet reaches p1's broadcast domain.
type UDPWaker struct{}

func (UDPWaker) Wake(mac, broadcast string) error {
	pkt, err := magicPacket(mac)
	if err != nil {
		return err
	}
	raddr, err := net.ResolveUDPAddr("udp", broadcast)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", broadcast, err)
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return fmt.Errorf("dial %q: %w", broadcast, err)
	}
	defer conn.Close()

	// Sending to a broadcast address requires SO_BROADCAST on the socket.
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	if setErr != nil {
		return fmt.Errorf("set SO_BROADCAST: %w", setErr)
	}

	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("send WoL: %w", err)
	}
	return nil
}

// magicPacket builds the 102-byte WoL payload: 6x 0xFF followed by the 6-byte
// MAC repeated 16 times.
func magicPacket(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("parse mac %q: %w", mac, err)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("mac %q is not 6 bytes", mac)
	}
	pkt := make([]byte, 6, 102)
	for i := range pkt {
		pkt[i] = 0xFF
	}
	for range 16 {
		pkt = append(pkt, hw...)
	}
	return pkt, nil
}
