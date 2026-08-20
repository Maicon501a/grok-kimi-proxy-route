// helloclient: connect with tls-client to the local dump server so its
// ClientHello gets captured.
package main

import (
	"flag"
	"fmt"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

func main() {
	prof := flag.String("profile", "146", "133|144|146")
	flag.Parse()
	p := profiles.Chrome_146
	if *prof == "133" {
		p = profiles.Chrome_133
	} else if *prof == "144" {
		p = profiles.Chrome_144
	}
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(10),
		tls_client.WithClientProfile(p),
		tls_client.WithInsecureSkipVerify(),
	)
	if err != nil {
		panic(err)
	}
	req, _ := http.NewRequest("GET", "https://127.0.0.1:18443/", nil)
	resp, err := hc.Do(req)
	if err != nil {
		fmt.Println("handshake result:", err)
		return
	}
	resp.Body.Close()
}
