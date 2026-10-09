package udev

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Sender multicasts monitor messages to the udev group of the network
// namespace it runs in. Sending to a multicast group needs CAP_NET_ADMIN,
// and libudev clients drop messages whose sender is not uid 0.
type Sender struct {
	fd int
}

// Open creates the netlink socket.
func Open() (*Sender, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, fmt.Errorf("netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("netlink bind: %w", err)
	}
	return &Sender{fd: fd}, nil
}

// Send multicasts one message to the udev group.
func (s *Sender) Send(msg []byte) error {
	if err := unix.Sendto(s.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: udevGroup}); err != nil {
		return fmt.Errorf("netlink send: %w", err)
	}
	return nil
}

// Close closes the socket.
func (s *Sender) Close() error { return unix.Close(s.fd) }
