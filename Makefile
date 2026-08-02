BINS := fleet-agent fleet-reconciler fc-supervisor fleet-gateway fleet-worker webhook-rx nats-bridge fleet-dispatch slack-gateway
DIST := bin

.PHONY: build test vet check dist clean

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

# Pre-deploy gate: vet + the full test suite.
check: vet test

# Cross-compile static linux/amd64 binaries for the fleet host (the Ansible deploy role
# copies these). fc-supervisor (M3) will be added here as CGO_ENABLED=0 too.
dist:
	@mkdir -p $(DIST)
	@for b in $(BINS); do \
	  echo "building $(DIST)/$$b (linux/amd64)"; \
	  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o $(DIST)/$$b ./cmd/$$b || exit 1; \
	done

clean:
	rm -rf $(DIST)
