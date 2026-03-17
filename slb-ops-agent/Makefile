BINARY     := slb-ops-agent
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u +%Y%m%dT%H%M%SZ)
LDFLAGS    := -X main.Version=$(VERSION) -s -w
GO         := go
BIN_DIR    := /opt/slb-agent/bin

.PHONY: all build proto clean install lint test

# ── 默认目标 ──────────────────────────────────────────────────────────────────
all: proto build

# ── Proto 代码生成 ────────────────────────────────────────────────────────────
# 依赖：protoc + protoc-gen-go + protoc-gen-go-grpc
proto:
	protoc \
		--go_out=pkg/proto --go_opt=paths=source_relative \
		--go-grpc_out=pkg/proto --go-grpc_opt=paths=source_relative \
		api/agent.proto

# ── 编译静态二进制（CGO_ENABLED=0 确保无动态链接）────────────────────────────
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	$(GO) build \
		-trimpath \
		-ldflags "$(LDFLAGS)" \
		-o dist/$(BINARY)_$(VERSION) \
		./cmd/agent
	@echo "Built: dist/$(BINARY)_$(VERSION)"
	@ls -lh dist/$(BINARY)_$(VERSION)

# ── 本地安装（开发/测试用）───────────────────────────────────────────────────
install: build
	install -d $(BIN_DIR)
	install -m 0755 dist/$(BINARY)_$(VERSION) $(BIN_DIR)/$(BINARY)_v$(VERSION)
	ln -sfn $(BIN_DIR)/$(BINARY)_v$(VERSION) $(BIN_DIR)/agent_current
	@echo "Installed and symlink updated: $(BIN_DIR)/agent_current -> $(BIN_DIR)/$(BINARY)_v$(VERSION)"

# ── 安装看门狗脚本 ────────────────────────────────────────────────────────────
install-watchdog:
	install -m 0755 scripts/watchdog.sh $(BIN_DIR)/watchdog.sh

# ── 单元测试 ──────────────────────────────────────────────────────────────────
test:
	$(GO) test -v -race ./...

# ── Lint ──────────────────────────────────────────────────────────────────────
lint:
	golangci-lint run ./...

# ── 清理 ──────────────────────────────────────────────────────────────────────
clean:
	rm -rf dist/

# ── 生成证书（开发自签证书，生产环境替换为真实 PKI）─────────────────────────
gen-dev-certs:
	@mkdir -p deploy/tls
	# CA
	openssl req -x509 -newkey rsa:4096 -nodes -days 3650 \
		-keyout deploy/tls/ca.key -out deploy/tls/ca.crt \
		-subj "/CN=slb-agent-dev-ca"
	# Agent cert（server + client dual purpose）
	openssl req -newkey rsa:4096 -nodes \
		-keyout deploy/tls/agent.key -out deploy/tls/agent.csr \
		-subj "/CN=slb-agent"
	openssl x509 -req -days 365 \
		-in deploy/tls/agent.csr \
		-CA deploy/tls/ca.crt -CAkey deploy/tls/ca.key -CAcreateserial \
		-out deploy/tls/agent.crt \
		-extfile <(printf "subjectAltName=IP:127.0.0.1\nextendedKeyUsage=serverAuth,clientAuth")
	@echo "Dev certs generated in deploy/tls/"
