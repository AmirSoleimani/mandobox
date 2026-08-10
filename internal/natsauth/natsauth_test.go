package natsauth

import (
	"testing"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

func decode(t *testing.T, creds string) *jwt.UserClaims {
	t.Helper()
	j, err := jwt.ParseDecoratedJWT([]byte(creds))
	if err != nil {
		t.Fatalf("parse creds: %v", err)
	}
	uc, err := jwt.DecodeUserClaims(j)
	if err != nil {
		t.Fatalf("decode user claims: %v", err)
	}
	return uc
}

func allows(uc *jwt.UserClaims, subj string) bool {
	in := func(l jwt.StringList) bool {
		for _, v := range l {
			if v == subj {
				return true
			}
		}
		return false
	}
	return in(uc.Permissions.Pub.Allow) || in(uc.Permissions.Sub.Allow)
}

func exactlyScoped(uc *jwt.UserClaims, subj string) bool {
	p, s := uc.Permissions.Pub.Allow, uc.Permissions.Sub.Allow
	return len(p) == 1 && p[0] == subj && len(s) == 1 && s[0] == subj
}

func TestSessionCredsAreConfinedAndSigned(t *testing.T) {
	ids, err := Generate()
	if err != nil {
		t.Fatal(err)
	}

	// Service cred → broad agent.>
	if svc := decode(t, ids.ServiceCreds); !exactlyScoped(svc, ServiceSubject) {
		t.Errorf("service creds not scoped to %s: pub=%v sub=%v", ServiceSubject, svc.Permissions.Pub.Allow, svc.Permissions.Sub.Allow)
	}

	// Per-session cred → agent.<sid>.> and NOTHING else.
	sid := "s_0123456789ABCDEFGHJKMNPQRS"
	creds, err := MintSessionCreds(ids.AccountSigningSeed, ids.AccountPubKey, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	uc := decode(t, creds)
	want := SessionSubject(sid)
	if !exactlyScoped(uc, want) {
		t.Fatalf("session creds not confined to %s: pub=%v sub=%v", want, uc.Permissions.Pub.Allow, uc.Permissions.Sub.Allow)
	}
	// The whole point: it must NOT reach the broad subtree, another session, or everything.
	for _, bad := range []string{"agent.>", "agent.s_OTHERSESSION0000000000000.>", ">", "agent.*.event"} {
		if allows(uc, bad) {
			t.Errorf("session creds wrongly allow %q", bad)
		}
	}

	// Signed by the account SIGNING key, on behalf of the account (issuer-account set).
	skKP, err := nkeys.FromSeed([]byte(ids.AccountSigningSeed))
	if err != nil {
		t.Fatal(err)
	}
	skPub, _ := skKP.PublicKey()
	if uc.Issuer != skPub {
		t.Errorf("user issuer = %s, want signing key %s", uc.Issuer, skPub)
	}
	if uc.IssuerAccount != ids.AccountPubKey {
		t.Errorf("issuer-account = %s, want account %s", uc.IssuerAccount, ids.AccountPubKey)
	}

	// Claims validate cleanly.
	var vr jwt.ValidationResults
	uc.Validate(&vr)
	if errs := vr.Errors(); len(errs) > 0 {
		t.Errorf("session user claims invalid: %v", errs)
	}

	// Two mints for the same session yield distinct user keys (fresh nkey each time).
	creds2, _ := MintSessionCreds(ids.AccountSigningSeed, ids.AccountPubKey, sid, 0)
	if decode(t, creds2).Subject == uc.Subject {
		t.Error("expected a fresh user nkey per mint")
	}
}
