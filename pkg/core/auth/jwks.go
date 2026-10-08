package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// errKeySource marks a failure to *reach* the key source (network, HTTP
// status, unparseable document) as opposed to a token that simply does not
// verify. The distinction matters: the first is our problem and deserves a
// 503, the second is the caller's and deserves a 401.
var errKeySource = errors.New("auth: key source unavailable")

// maxJWKSBytes caps the JWKS response we are willing to read.
const maxJWKSBytes = 1 << 20 // 1MB

// defaultJWKSClient is used when Config.JWKSClient is nil.
var defaultJWKSClient = &http.Client{Timeout: 10 * time.Second}

// keySet fetches and caches a JWKS document.
type keySet struct {
	url    string
	ttl    time.Duration
	client *http.Client

	mu      sync.RWMutex
	keys    map[string]any // kid → public key
	fetched time.Time
	gen     uint64 // refresh counter: lets a waiter tell "someone else refreshed"
}

func newKeySet(url string, ttl time.Duration, client *http.Client) *keySet {
	if client == nil {
		client = defaultJWKSClient
	}
	return &keySet{url: url, ttl: ttl, client: client}
}

// key returns the public key for (alg, kid). A kid the cache does not know
// is treated as a possible rotation: the document is refetched once before
// giving up, so rotation works without waiting out the TTL.
func (s *keySet) key(ctx context.Context, alg, kid string) (any, error) {
	keys, err := s.load(ctx, false)
	if err != nil {
		return nil, err
	}
	if k, ok := lookup(keys, kid); ok {
		return k, nil
	}
	keys, err = s.load(ctx, true)
	if err != nil {
		return nil, err
	}
	if k, ok := lookup(keys, kid); ok {
		return k, nil
	}
	if kid == "" {
		return nil, fmt.Errorf("%w: no key for alg %s", errKeySource, alg)
	}
	return nil, fmt.Errorf("%w: no key for kid %q", errKeySource, kid)
}

// load returns the cached key map, refetching when it is stale, when force
// is set, or when nothing has been fetched yet.
//
// Concurrent callers serialize on the write lock and a waiter reuses the
// result the winner just produced (detected via the refresh counter), so a
// burst of requests cannot stampede the identity provider — while a
// genuinely forced refetch, which is how key rotation is picked up, still
// happens.
func (s *keySet) load(ctx context.Context, force bool) (map[string]any, error) {
	s.mu.RLock()
	keys, fetched, gen := s.keys, s.fetched, s.gen
	s.mu.RUnlock()
	if keys != nil && !force && time.Since(fetched) < s.ttl {
		return keys, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen && s.keys != nil {
		return s.keys, nil // someone refreshed while we waited for the lock
	}
	keys, err := s.fetch(ctx)
	if err != nil {
		// A briefly unreachable provider should not take every request
		// down: keep serving the last good key set. Only a verifier that
		// has never fetched successfully surfaces the error.
		if s.keys != nil {
			return s.keys, nil
		}
		return nil, err
	}
	s.keys, s.fetched, s.gen = keys, time.Now(), s.gen+1
	return keys, nil
}

// fetch downloads and parses the JWKS document.
func (s *keySet) fetch(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errKeySource, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errKeySource, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned %d", errKeySource, s.url, resp.StatusCode)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: decode jwks: %v", errKeySource, err)
	}
	if len(doc.Keys) == 0 {
		return nil, fmt.Errorf("%w: jwks has no keys", errKeySource)
	}
	keys := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		pub, err := k.publicKey()
		if err != nil {
			// One bad key must not poison the whole set.
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: jwks has no usable keys", errKeySource)
	}
	return keys, nil
}

// lookup finds a key by kid. A document with exactly one unnamed key is
// accepted for tokens without a kid (common for small deployments).
func lookup(keys map[string]any, kid string) (any, bool) {
	if kid != "" {
		k, ok := keys[kid]
		return k, ok
	}
	if len(keys) == 1 {
		for _, k := range keys {
			return k, true
		}
	}
	return nil, false
}

// jwk is one JSON Web Key. Only the fields needed to rebuild an RSA or EC
// public key are decoded; symmetric (oct) keys are deliberately not
// trusted from a remote document.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`

	// RSA
	N string `json:"n"`
	E string `json:"e"`

	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicKey rebuilds the crypto public key from its JWK coordinates.
func (k jwk) publicKey() (any, error) {
	switch k.Kty {
	case "RSA":
		n, err := decodeB64(k.N)
		if err != nil {
			return nil, err
		}
		e, err := decodeB64(k.E)
		if err != nil {
			return nil, err
		}
		exp := new(big.Int).SetBytes(e)
		if !exp.IsInt64() || exp.Int64() <= 1 {
			return nil, errors.New("auth: invalid RSA exponent")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, nil
	case "EC":
		curve, err := namedCurve(k.Crv)
		if err != nil {
			return nil, err
		}
		x, err := decodeB64(k.X)
		if err != nil {
			return nil, err
		}
		y, err := decodeB64(k.Y)
		if err != nil {
			return nil, err
		}
		// Reject off-curve points: a bad point can leak the private key
		// when verification is attempted.
		if !curve.IsOnCurve(new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)) {
			return nil, errors.New("auth: EC point is not on the curve")
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(x),
			Y:     new(big.Int).SetBytes(y),
		}, nil
	default:
		return nil, fmt.Errorf("auth: unsupported key type %q", k.Kty)
	}
}

func namedCurve(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("auth: unsupported curve %q", crv)
	}
}

func decodeB64(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("auth: empty jwk field")
	}
	return base64.RawURLEncoding.DecodeString(s)
}
