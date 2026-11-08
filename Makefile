PREFIX ?= $(HOME)/.local
HRX_REVISION := $(shell git describe --always --dirty --abbrev=12 2>/dev/null || echo unknown)

.PHONY: build test test-unit test-native test-pitput test-pitput-bridge test-pitput-dm test-pitput-opencode test-pitput-targeted test-hub-pitput test-broker test-dashboard test-all install install-broker

build:
	go build -trimpath -ldflags '-X github.com/meshcrawler/hrx/internal/app.buildRevision=$(HRX_REVISION)' -o bin/hrx ./cmd/hrx

test: test-unit

test-unit:
	HRX_PROBE_MODEL= HRX_NATIVE_SESSION_TESTS= go test -race -count=1 -timeout=10m ./...
	go vet ./...
	uv run --no-project python -m unittest discover -s internal/app -p test_yesod_register.py
	uv run --no-project python -m unittest discover -s services/gitlab-service-account-broker

test-broker:
	go test -race ./internal/app -run '^TestBroker' -count=1
	uv run --no-project python -m unittest discover -s services/gitlab-service-account-broker -v

test-dashboard:
	go test -race ./internal/monitor ./internal/session
	go test -race ./internal/app -run '^TestDashboard|^TestSessionHook|^TestAttachSession' -count=1

install-broker:
	install -d "$(PREFIX)/bin"
	install -m 0755 services/gitlab-service-account-broker/broker.py "$(PREFIX)/bin/gitlab-service-account-broker"

test-native:
	HRX_NATIVE_SESSION_TESTS=1 go test -race ./internal/app -run '^TestNative' -count=1 -timeout=5m

# PITPUT_SOURCE is an explicit local checkout; the temporary uv environment
# includes server dependencies without changing the installed Pitput CLI.
test-pitput: build
	test -n "$(PITPUT_SOURCE)" || (echo 'Set PITPUT_SOURCE to a local Pitput checkout'; exit 1)
	go build -trimpath -o bin/hrx-integration ./tests/integration/hrxfixture
	uv run --no-project --with "$(PITPUT_SOURCE)[server]" python tests/integration/pitput_exchange.py --hrx "$(CURDIR)/bin/hrx-integration"

test-pitput-bridge:
	test -n "$(PITPUT_SOURCE)" || (echo 'Set PITPUT_SOURCE to a local Pitput checkout'; exit 1)
	uv run --no-project --with "$(PITPUT_SOURCE)[server]" python tests/integration/pitput_codex_bridge.py

test-pitput-dm:
	test -n "$(PITPUT_SOURCE)" || (echo 'Set PITPUT_SOURCE to a local Pitput checkout'; exit 1)
	uv run --no-project --with "$(PITPUT_SOURCE)[server]" python tests/integration/pitput_dm_bridge.py

test-hub-pitput:
	test -n "$(PITPUT_SOURCE)" || (echo 'Set PITPUT_SOURCE to a local Pitput checkout'; exit 1)
	go build -trimpath -o bin/hrx-integration ./tests/integration/hrxfixture
	uv run --no-project --with "$(PITPUT_SOURCE)[server]" python tests/integration/hub_pitput.py --hrx "$(CURDIR)/bin/hrx-integration"

test-pitput-opencode:
	test -n "$(PITPUT_SOURCE)" || (echo 'Set PITPUT_SOURCE to a local Pitput checkout'; exit 1)
	python3 tests/integration/opencode_queue.py
	uv run --no-project --with "$(PITPUT_SOURCE)[server]" python tests/integration/pitput_opencode_bridge.py

# Optional HRX_PITPUT_SOURCE_SHA (full 40-hex commit) plus HRX_PITPUT_SOURCE_URL runs
# this suite against a wheel built from exactly that Pitput commit instead.
test-pitput-targeted:
	test -n "$(PITPUT_SOURCE)" || (echo 'Set PITPUT_SOURCE to a local Pitput checkout'; exit 1)
	uv run --no-project --with "$(PITPUT_SOURCE)[server]" python tests/integration/pitput_targeted_launch.py

test-all: test-unit test-native test-pitput

install: build
	install -d "$(PREFIX)/bin" "$(PREFIX)/share/zsh/site-functions"
	install -m 0755 bin/hrx "$(PREFIX)/bin/hrx"
	bin/hrx completion zsh > "$(PREFIX)/share/zsh/site-functions/_hrx"
