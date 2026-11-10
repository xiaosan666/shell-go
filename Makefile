.PHONY: help build build-all package test fmt vet run clean

APP      := shell-go
VERSION  := $(shell cat VERSION 2>/dev/null || echo 1.0.0)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.buildVersion=$(VERSION) -X main.buildCommit=$(COMMIT)

help:
	@echo "make build     编译本机版本（开发调试）"
	@echo "make build-all 交叉编译 linux/amd64 与 linux/arm64"
	@echo "make package   生成 dist/ 下的发布包"
	@echo "make test      运行单元测试"
	@echo "make run       使用 .dev/config.yaml 本地启动"
	@echo "make fmt       格式化代码"
	@echo "make vet       静态检查"
	@echo "make clean     清理构建产物"

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(APP) .

build-all:
	./build.sh

package:
	./build.sh all

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

vet:
	go vet ./...

run: build
	./$(APP) -config .dev/config.yaml

clean:
	rm -f $(APP)
	rm -rf dist
