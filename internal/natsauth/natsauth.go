// Package natsauth implements NATS decentralized (JWT) auth for the fleet control bus, so each guest
// is confined to its OWN subject subtree (agent.<session_id>.>) and cannot read, inject, or forge on
// any other session's streams (Tier-1 scoping; closes the "unauthenticated NATS" audit
// finding). One operator → one account (with a signing key) → users:
//
//   - a static SERVICE user (agent.>) for the worker + nats-bridge, generated once at provision time;
//   - a per-session user (agent.<sid>.>) minted at launch by the control plane, which holds the
//     account SIGNING seed (a Tier-0 secret). The guest already consumes the resulting .creds file.
//
// The server runs in operator mode with the account JWT preloaded (MEMORY resolver) and no anonymous
// user, so a credential-less connection is refused.
package natsauth

import (
	"fmt"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// ServiceSubject is the broad subtree the worker + nats-bridge operate over (all sessions).
const ServiceSubject = "agent.>"

// SessionSubject returns the subtree a single session's guest is confined to.
func SessionSubject(sessionID string) string { return "agent." + sessionID + ".>" }

// Identities are the static artifacts generated once at provision time.
type Identities struct {
	OperatorJWT        string // NATS server config: `operator`
	AccountPubKey      string // NATS server config: resolver_preload key
	AccountJWT         string // NATS server config: resolver_preload value
	AccountSigningSeed string // Tier-0 secret the control plane holds to mint per-session user creds
	ServiceCreds       string // .creds for the worker + nats-bridge (agent.> pub/sub)
}

// Generate creates a fresh operator → account (+ signing key) → service-user chain.
func Generate() (*Identities, error) {
	opKP, err := nkeys.CreateOperator()
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	opPub, err := opKP.PublicKey()
	if err != nil {
		return nil, err
	}
	accKP, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	accPub, err := accKP.PublicKey()
	if err != nil {
		return nil, err
	}
	// A separate signing key: users are signed by it, so the account IDENTITY seed can be discarded and
	// only the (less-privileged) signing seed is held by the control plane to mint session creds.
	skKP, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	skPub, err := skKP.PublicKey()
	if err != nil {
		return nil, err
	}
	skSeed, err := skKP.Seed()
	if err != nil {
		return nil, err
	}

	oc := jwt.NewOperatorClaims(opPub)
	oc.Name = "mandobox"
	opJWT, err := oc.Encode(opKP)
	if err != nil {
		return nil, fmt.Errorf("encode operator: %w", err)
	}

	ac := jwt.NewAccountClaims(accPub)
	ac.Name = "fleet"
	ac.SigningKeys.Add(skPub)
	accJWT, err := ac.Encode(opKP)
	if err != nil {
		return nil, fmt.Errorf("encode account: %w", err)
	}

	svc, err := mintUserCreds(skKP, accPub, "service", ServiceSubject, 0)
	if err != nil {
		return nil, fmt.Errorf("service creds: %w", err)
	}

	return &Identities{
		OperatorJWT:        opJWT,
		AccountPubKey:      accPub,
		AccountJWT:         accJWT,
		AccountSigningSeed: string(skSeed),
		ServiceCreds:       svc,
	}, nil
}

// MintSessionCreds mints a per-session user .creds confined to agent.<sessionID>.>, signed by the
// account signing key (the control plane holds its seed). ttl 0 = no expiry (creds live only as long
// as the guest VM, which is destroyed after the session; the file is tmpfs).
func MintSessionCreds(accountSigningSeed, accountPubKey, sessionID string, ttl time.Duration) (string, error) {
	skKP, err := nkeys.FromSeed([]byte(accountSigningSeed))
	if err != nil {
		return "", fmt.Errorf("account signing seed: %w", err)
	}
	return mintUserCreds(skKP, accountPubKey, sessionID, SessionSubject(sessionID), ttl)
}

// mintUserCreds signs a user JWT scoped to exactly `subject` (pub AND sub) and returns the .creds file
// content (JWT + user seed).
func mintUserCreds(signingKP nkeys.KeyPair, accountPub, name, subject string, ttl time.Duration) (string, error) {
	uKP, err := nkeys.CreateUser()
	if err != nil {
		return "", err
	}
	uPub, err := uKP.PublicKey()
	if err != nil {
		return "", err
	}
	uSeed, err := uKP.Seed()
	if err != nil {
		return "", err
	}
	uc := jwt.NewUserClaims(uPub)
	uc.Name = name
	uc.IssuerAccount = accountPub // required when the user is signed by an account SIGNING key
	uc.Permissions.Pub.Allow.Add(subject)
	uc.Permissions.Sub.Allow.Add(subject)
	if ttl > 0 {
		uc.Expires = time.Now().Add(ttl).Unix()
	}
	uJWT, err := uc.Encode(signingKP)
	if err != nil {
		return "", err
	}
	creds, err := jwt.FormatUserConfig(uJWT, uSeed)
	if err != nil {
		return "", err
	}
	return string(creds), nil
}
