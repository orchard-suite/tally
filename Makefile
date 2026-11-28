SHELL := /bin/bash
VERSION := $(shell cut -d' ' -f1 VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
IMAGE := ghcr.io/orchard-suite/tally

.PHONY: check test build docker run land install-ship install-rotation

# Everything that must pass before a commit.
check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: the files above need formatting" >&2; exit 1; }
	go vet ./...
	go test ./...
	./claude-rotation_test.sh

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/tally ./cmd/tally

docker:
	docker build --platform linux/amd64 -t $(IMAGE):$(VERSION) .

# A local board on :8080 (behind the door's headers) and :8081 (health),
# reading a Prometheus on :19090, for example one port-forwarded with
#   kubectl -n monitoring port-forward svc/prometheus 19090:9090
# Open it with the door's headers:
#   curl -H 'X-Forwarded-Email: you@example.com' localhost:8080/v1/costs
run: build
	PROMETHEUS_URL=http://127.0.0.1:19090 ./dist/tally serve

# This Mac's shipper: builds tally into ~/.local/bin and (re)starts the
# launch agent SHIP_LABEL, posting to TALLY_URL. It needs the machine's
# token in ~/.config/tally/ship-token and the token's sha256 in that
# tally's SHIP_TOKENS. A Mac that posts to an employer's tally posts only
# the work its AI gateway served:
#   make install-ship TALLY_URL=https://tally.example.com SHIP_ROUTE=ai.example.com
TALLY_URL ?=
SHIP_ROUTE ?=
SHIP_LABEL ?= tally.ship
SHIP_PLIST := $(HOME)/Library/LaunchAgents/$(SHIP_LABEL).plist
install-ship:
	@test -n "$(TALLY_URL)" || { echo "install-ship: set TALLY_URL to the tally this Mac posts to" >&2; exit 1; }
	@test -s $(HOME)/.config/tally/ship-token || { echo "install-ship: put this machine's token in ~/.config/tally/ship-token" >&2; exit 1; }
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(HOME)/.local/bin/tally ./cmd/tally
	@# `tally ship cost` runs from an agent's bridge with none of the plist's settings; it reads the URL here.
	printf '%s\n' "$(TALLY_URL)" > $(HOME)/.config/tally/ship-url
	mkdir -p $(HOME)/Library/LaunchAgents $(HOME)/Library/Logs
	sed -e "s|__LABEL__|$(SHIP_LABEL)|g" -e "s|__HOME__|$(HOME)|g" -e "s|__TALLY_URL__|$(TALLY_URL)|g" -e "s|__SHIP_ROUTE__|$(SHIP_ROUTE)|g" deploy/macos/tally-ship.plist > $(SHIP_PLIST)
	launchctl bootout gui/$$(id -u)/$(SHIP_LABEL) 2>/dev/null || true
	@# bootout returns before launchd lets go of the label; bootstrap fails until it does.
	@for i in $$(seq 50); do launchctl print gui/$$(id -u)/$(SHIP_LABEL) >/dev/null 2>&1 || break; sleep 0.2; done
	launchctl bootstrap gui/$$(id -u) $(SHIP_PLIST)

# The build-account rotation's command, into ~/.local/bin. Its settings
# are in ~/.config/tally/rotation.env or the environment.
install-rotation:
	install -m 755 claude-rotation $(HOME)/.local/bin/claude-rotation

# Lands main: release-damnit land runs the config's check (make check) on
# exactly what lands and signs its proof, which the release verifies. From a
# worktree: make land BRANCH=<branch>.
land:
	release-damnit land $(BRANCH)
