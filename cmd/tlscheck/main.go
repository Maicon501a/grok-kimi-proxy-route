// tlscheck: fetch tls.peet.ws/api/all with the probe's tls-client and print
// the fingerprint Google would see (JA3/JA4/H2). Compare with real Chrome.
package main

import (
	"flag"
	"fmt"
	"io"

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
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(p),
		tls_client.WithInsecureSkipVerify(), // echo test only
	)
	if err != nil {
		panic(err)
	}
	req, _ := http.NewRequest("GET", "https://tls.peet.ws/api/all", nil)
	resp, err := hc.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Println(string(b))
}
