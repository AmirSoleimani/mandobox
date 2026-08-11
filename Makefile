BINS := mando-agent fc-supervisor mando-gateway mando-worker webhook-rx nats-bridge mando-dispatch mando-connectors mando-natsauth
DIST := bin

.PHONY: build test vet check dist dist-dashboard test-dashboard clean

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

# Pre-deploy gate: vet + the full test suite (both modules).
check: vet test test-dashboard

# The dashboard is a separate Go module (its own go.mod) — test it from its own directory.
test-dashboard:
	cd dashboard && go test ./...

# Cross-compile static linux/amd64 binaries for the fleet host (the Ansible deploy roles copy
# these). Includes the separate dashboard module.
dist: dist-dashboard
	@mkdir -p $(DIST)
	@for b in $(BINS); do \
	  echo "building $(DIST)/$$b (linux/amd64)"; \
	  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o $(DIST)/$$b ./cmd/$$b || exit 1; \
	done

# The dashboard lives in its own module, so build it from that directory into the shared dist dir.
dist-dashboard:
	@mkdir -p $(DIST)
	@echo "building $(DIST)/mando-dashboard (linux/amd64)"
	@cd dashboard && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o ../$(DIST)/mando-dashboard .

clean:
	rm -rf $(DIST)
