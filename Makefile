# go-tmux-saver developer tasks.
#
# On a host that also runs a PRODUCTION tmux server (ten64, big-storage, a
# workstation), use `make test-sandboxed` rather than a bare `go test`: it
# bounds CPU/memory and relocates the tmux socket directory so the tests
# cannot touch your real servers. See scripts/sandboxed-test.py.

GO ?= go

.PHONY: test test-sandboxed vet build

# Plain test run (CI, throwaway dev boxes). The tests isolate their tmux
# sockets themselves (see internal/tmuxtest), but this has no CPU/memory cap.
test:
	$(GO) test ./...

# Resource-limited, tmux-isolated test run for hosts with a live tmux server.
# Extra args pass through to `go test`, e.g.:
#   make test-sandboxed GOTESTFLAGS='-run TestSave -count=1 ./internal/cli'
test-sandboxed:
	./scripts/sandboxed-test.py -- $(GOTESTFLAGS)

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...
