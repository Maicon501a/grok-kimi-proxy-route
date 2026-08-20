// hellodump: raw TCP listener that prints the first TLS record (ClientHello)
// of each connection as hex, then closes. Used to diff Go tls-client vs Chrome.
package main

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:18443")
	if err != nil {
		panic(err)
	}
	fmt.Println("listening on 127.0.0.1:18443")
	label := os.Args[1:]
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 4096)
			n, _ := c.Read(buf)
			if n == 0 {
				return
			}
			name := "hello"
			if len(label) > 0 {
				name = label[0]
			}
			os.WriteFile("tmp_probe/"+name+".hex", []byte(hex.EncodeToString(buf[:n])), 0o600)
			fmt.Printf("%s: %d bytes captured\n", name, n)
		}(c)
	}
}
