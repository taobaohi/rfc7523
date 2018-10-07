package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"net/http/httputil"

	"github.com/coreos/go-oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	jose "gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"
)

func main() {
	// json web key setup - JWK

	bits := 2048
	pk, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		log.Fatal(err)
	}

	use := "sig"
	sigAlg := jose.RS256
	var kid string
	{
		b := make([]byte, 5)
		_, err := rand.Read(b)
		if err != nil {
			log.Fatal(err)
		}
		kid = base32.StdEncoding.EncodeToString(b)
	}

	privJWK := jose.JSONWebKey{
		Key:       pk,
		KeyID:     kid,
		Use:       use,
		Algorithm: string(sigAlg),
	}

	pubJWK := jose.JSONWebKey{
		Key:       pk.Public(),
		KeyID:     kid,
		Use:       use,
		Algorithm: string(sigAlg),
	}

	pubJWKS := jose.JSONWebKeySet{[]jose.JSONWebKey{pubJWK}}

	// the `/jwks` endpoint hosting the JWK public key content

	http.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		{
			rd, _ := httputil.DumpRequest(r, true)
			log.Println("/jwks handler: request", string(rd))
		}
		_ = json.NewEncoder(w).Encode(pubJWKS)
	})

	go func() {
		http.ListenAndServe(":8888", nil)
	}()

	signer, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: sigAlg,
			Key:       privJWK,
		},
		&jose.SignerOptions{
			// this will only embed the JWK's key ID "kid"
			// the public key content is retrieved using the `/jwks` endpoint
			EmbedJWK: false,
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	// oauth

	issuer := "http://localhost:8080/auth/realms/master"

	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		log.Fatal(err)
	}

	// no client ID and client secrets needed here,
	// as the client is asserted via a signed jwt.
	cfg := clientcredentials.Config{TokenURL: provider.Endpoint().TokenURL}

	transport := &http.Transport{
		Dial:                (&net.Dialer{Timeout: 10 * time.Second}).Dial,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     30 * time.Second,
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &debugRoundTripper{
			&jwtClientAuthenticator{
				claims: Claims{
					// subject needs to match the client ID,
					// see https://github.com/keycloak/keycloak/blob/b478472b3578b8980d7b5f1642e91e75d1e78d16/services/src/main/java/org/keycloak/authentication/authenticators/client/JWTClientAuthenticator.java#L102-L105
					Subject: "telemeter",

					// audience needs to match realm issuer,
					// see https://github.com/keycloak/keycloak/blob/b478472b3578b8980d7b5f1642e91e75d1e78d16/services/src/main/java/org/keycloak/authentication/authenticators/client/JWTClientAuthenticator.java#L142-L144
					Audience: []string{issuer},
				},

				signer: signer,
				next:   transport,
			},
		},
	}

	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)

	src := cfg.TokenSource(ctx)
	for {
		tok, err := src.Token()
		if err != nil {
			log.Fatal(err)
		}

		log.Println("--- Access Token Expiry")
		log.Println(tok.Expiry)
		log.Println("--- Access Token")
		fmt.Println(tok.AccessToken)
		log.Println("--- Refresh Token")
		fmt.Println(tok.RefreshToken)

		fmt.Println("retrying in 1 minute and 30 seconds")
		time.Sleep(30 * time.Second)
	}
}

type debugRoundTripper struct {
	next http.RoundTripper
}

func (rt *debugRoundTripper) RoundTrip(req *http.Request) (res *http.Response, err error) {
	res, err = rt.next.RoundTrip(req)
	if err != nil {
		log.Println(err)
		return
	}

	reqd, _ := httputil.DumpRequest(req, true)
	log.Println("request", string(reqd))

	resd, _ := httputil.DumpResponse(res, true)
	log.Println("response", string(resd))

	return
}

type Claims struct {
	Issuer   string
	Subject  string
	Audience []string
	ID       string
}

type jwtClientAuthenticator struct {
	claims Claims
	signer jose.Signer
	next   http.RoundTripper
}

func (rt *jwtClientAuthenticator) RoundTrip(req *http.Request) (*http.Response, error) {
	clientAuthClaims := jwt.Claims{
		Issuer:   rt.claims.Issuer,
		Subject:  rt.claims.Subject,
		Audience: rt.claims.Audience,
		ID:       rt.claims.ID,
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}

	clientAuthJWT, err := jwt.Signed(rt.signer).Claims(clientAuthClaims).CompactSerialize()
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Del("Authorization") // replaced with client assertion

	if err := req.ParseForm(); err != nil {
		return nil, err
	}

	req.Form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	req.Form.Set("client_assertion", clientAuthJWT)

	newBody := req.Form.Encode()
	req.Body = ioutil.NopCloser(strings.NewReader(string(newBody)))
	req.ContentLength = int64(len(newBody))

	return rt.next.RoundTrip(req)
}

func mustMarshal(src json.Marshaler) []byte {
	bytes, err := src.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return bytes
}
