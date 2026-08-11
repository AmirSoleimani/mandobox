// Command mando-natsauth generates the one-time NATS decentralized-auth material for the fleet control
// bus (operator, account + signing key, service credential). Run it ONCE on the controller; the
// outputs are Tier-0 secrets that Ansible distributes to the box. The account SIGNING seed lets the
// worker mint per-session guest creds confined to agent.<sid>.>.
//
// Regenerating invalidates every existing credential — only do so as a deliberate rotation.
//
//	mando-natsauth -out secrets/nats
//	mando-natsauth -probe nats://172.31.0.1:4222 -account-seed /etc/fleet/nats-account.seed \
//	    -account-pub <pub>   # isolation self-test (run ON the box): a scoped cred must be denied
//	                         # cross-session subscribe/publish.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AmirSoleimani/mandobox/internal/natsauth"
	"github.com/nats-io/nats.go"
)

func main() {
	out := flag.String("out", ".", "directory to write the auth artifacts into")
	probe := flag.String("probe", "", "isolation self-test: NATS URL to connect to with a freshly minted scoped cred")
	accountSeedFile := flag.String("account-seed", "", "account signing seed file (for -probe)")
	accountPub := flag.String("account-pub", "", "account public key (for -probe)")
	flag.Parse()

	if *probe != "" {
		runProbe(*probe, *accountSeedFile, *accountPub)
		return
	}
	generate(*out)
}

func generate(out string) {
	ids, err := natsauth.Generate()
	if err != nil {
		log.Fatalf("generate: %v", err)
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		log.Fatalf("mkdir %s: %v", out, err)
	}
	// .jwt/.pub are public (server config + issuer-account). .seed/.creds are Tier-0 (0600).
	files := []struct {
		name    string
		content string
		mode    os.FileMode
	}{
		{"nats-operator.jwt", ids.OperatorJWT, 0o644},
		{"nats-account.jwt", ids.AccountJWT, 0o644},
		{"nats-account.pub", ids.AccountPubKey, 0o644},
		{"nats-account.seed", ids.AccountSigningSeed, 0o600},
		{"nats-service.creds", ids.ServiceCreds, 0o600},
	}
	for _, f := range files {
		p := filepath.Join(out, f.name)
		if err := os.WriteFile(p, []byte(f.content+"\n"), f.mode); err != nil {
			log.Fatalf("write %s: %v", p, err)
		}
		fmt.Printf("wrote %s (%#o)\n", p, f.mode)
	}
	fmt.Printf("\naccount public key: %s\n", ids.AccountPubKey)
}

// runProbe mints a per-session cred and verifies the SERVER confines it: its own subtree is allowed,
// everything else (the broad tree, another session, another session's command) is denied.
func runProbe(url, seedFile, accountPub string) {
	seed, err := os.ReadFile(seedFile)
	if err != nil {
		log.Fatalf("read account seed: %v", err)
	}
	const sid = "s_PROBE00000000000000000000" // valid session_id shape
	creds, err := natsauth.MintSessionCreds(strings.TrimSpace(string(seed)), accountPub, sid, 0)
	if err != nil {
		log.Fatalf("mint: %v", err)
	}
	credFile := filepath.Join(os.TempDir(), "natsauth-probe.creds")
	if err := os.WriteFile(credFile, []byte(creds), 0o600); err != nil {
		log.Fatalf("write cred: %v", err)
	}
	defer os.Remove(credFile)

	var mu sync.Mutex
	var violations []string
	nc, err := nats.Connect(url, nats.UserCredentials(credFile), nats.Timeout(5*time.Second),
		nats.ErrorHandler(func(_ *nats.Conn, s *nats.Subscription, e error) {
			mu.Lock()
			subj := ""
			if s != nil {
				subj = s.Subject
			}
			violations = append(violations, fmt.Sprintf("%v (%s)", e, subj))
			mu.Unlock()
		}))
	if err != nil {
		log.Fatalf("connect with scoped cred FAILED (auth broken?): %v", err)
	}
	defer nc.Close()

	own := "agent." + sid + ".event"
	_, _ = nc.SubscribeSync(own)                                              // must be allowed
	_, _ = nc.SubscribeSync("agent.>")                                        // must be denied
	_, _ = nc.SubscribeSync("agent.s_OTHERSESSION00000000000000.event")       // must be denied
	_ = nc.Publish("agent.s_OTHERSESSION00000000000000.command", []byte("x")) // must be denied
	_ = nc.Flush()
	time.Sleep(700 * time.Millisecond) // let async permission violations arrive

	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("connected with a cred scoped to agent.%s.>\n", sid)
	fmt.Printf("server-side permission violations (expect ≥3 — the cross-session attempts):\n")
	for _, v := range violations {
		fmt.Printf("  DENIED: %s\n", v)
	}
	if len(violations) == 0 {
		fmt.Println("  NONE — ISOLATION NOT ENFORCED (this is a failure!)")
		os.Exit(1)
	}
	fmt.Println("PASS: the scoped cred is confined to its own subtree.")
}
