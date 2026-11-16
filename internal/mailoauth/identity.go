package mailoauth

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var tenantID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

const consumerTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

func verifyIdentity(ctx context.Context, client *http.Client, app App, encoded, nonce string) (Identity, error) {
	fail := func() (Identity, error) { return Identity{}, Failure("oauth_identity_invalid") }
	parts := strings.Split(encoded, ".")
	if len(encoded) > 65536 || len(parts) != 3 {
		return fail()
	}
	decode := func(part string, value any) bool {
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		return err == nil && json.Unmarshal(decoded, value) == nil
	}
	var header struct {
		Algorithm string `json:"alg"`
		Key       string `json:"kid"`
	}
	var claims struct {
		Issuer            string `json:"iss"`
		Audience          string `json:"aud"`
		Subject           string `json:"sub"`
		ObjectID          string `json:"oid"`
		Tenant            string `json:"tid"`
		Nonce             string `json:"nonce"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Verified          bool   `json:"email_verified"`
		Expires           int64  `json:"exp"`
		Issued            int64  `json:"iat"`
		NotBefore         int64  `json:"nbf"`
	}
	if !decode(parts[0], &header) || !decode(parts[1], &claims) || header.Algorithm != "RS256" || header.Key == "" || claims.Subject == "" || len(claims.Subject) > 512 || claims.Audience != app.ClientID || nonce == "" || claims.Nonce != nonce || claims.Expires <= time.Now().Unix() || claims.Issued > time.Now().Add(time.Minute).Unix() || claims.NotBefore > time.Now().Add(time.Minute).Unix() {
		return fail()
	}
	if app.Provider == "google" {
		if (claims.Issuer != "https://accounts.google.com" && claims.Issuer != "accounts.google.com") || !claims.Verified || claims.Email == "" {
			return fail()
		}
	} else {
		if !tenantID.MatchString(claims.Tenant) || claims.Issuer != "https://login.microsoftonline.com/"+claims.Tenant+"/v2.0" || (app.Authority == "consumers" && claims.Tenant != consumerTenant) {
			return fail()
		}
		if claims.ObjectID != "" && !tenantID.MatchString(claims.ObjectID) {
			return fail()
		}
	}
	_, _, endpoint := endpoints(app)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fail()
	}
	response, err := client.Do(request)
	if err != nil {
		return Identity{}, Failure("oauth_network_failed")
	}
	var jwks struct {
		Keys []struct {
			Type      string `json:"kty"`
			Key       string `json:"kid"`
			Algorithm string `json:"alg"`
			Use       string `json:"use"`
			Modulus   string `json:"n"`
			Exponent  string `json:"e"`
			Issuer    string `json:"issuer"`
		} `json:"keys"`
	}
	if err = readJSON(response, &jwks); err != nil {
		return Identity{}, err
	}
	if response.StatusCode != 200 {
		return fail()
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fail()
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, key := range jwks.Keys {
		if key.Key != header.Key || key.Type != "RSA" || (key.Algorithm != "" && key.Algorithm != "RS256") || (key.Use != "" && key.Use != "sig") {
			continue
		}
		if key.Issuer != "" && strings.ReplaceAll(key.Issuer, "{tenantid}", claims.Tenant) != claims.Issuer {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(key.Modulus)
		if err != nil || len(modulus) < 256 || len(modulus) > 1024 {
			continue
		}
		exponent, err := base64.RawURLEncoding.DecodeString(key.Exponent)
		if err != nil || len(exponent) < 1 || len(exponent) > 4 {
			continue
		}
		public := rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(new(big.Int).SetBytes(exponent).Int64())}
		if public.E < 3 || public.E%2 == 0 {
			continue
		}
		if rsa.VerifyPKCS1v15(&public, crypto.SHA256, digest[:], signature) == nil {
			email := claims.Email
			if app.Provider == "microsoft" && email == "" {
				email = claims.PreferredUsername
			}
			subject := claims.Subject
			if app.Provider == "microsoft" && claims.ObjectID != "" {
				subject = "oid:" + strings.ToLower(claims.ObjectID)
			}
			return Identity{Subject: subject, Tenant: claims.Tenant, Email: email}, nil
		}
	}
	return fail()
}
