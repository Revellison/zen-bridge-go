package config

import (
	"errors"
	"net"
	"os"
)

const defaultListen = "127.0.0.1:18788"

func ListenAddress() (string, error) {
	addr := os.Getenv("ZEN_BRIDGE_LISTEN")
	if addr == "" {
		addr = defaultListen
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", errors.New("ZEN_BRIDGE_LISTEN must be a host:port address")
	}
	return addr, nil
}
