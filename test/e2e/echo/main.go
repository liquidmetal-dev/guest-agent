package main

import (
	"io"
	"log"
	"net"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:2222")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("echo helper listening on %s", l.Addr())

	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatalf("accept: %v", err)
		}
		go func(c net.Conn) {
			defer c.Close()
			_, _ = io.Copy(c, c)
		}(c)
	}
}
