BIN := tvhub
ifeq ($(OS),Windows_NT)
BIN := tvhub.exe
endif

VERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w

.PHONY: build run test fmt vet clean dist

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

run: build
	./$(BIN) -data ./data

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -rf dist $(BIN) tvhub.exe data

# 交叉编译：一条命令出全部发布产物（不需要 cgo/gcc）
dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/tvhub-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/tvhub-linux-arm64 .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm   GOARM=7 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/tvhub-linux-armv7 .
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/tvhub-windows-amd64.exe .
	@ls -lh dist
