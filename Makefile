# qbhive —— 目标约定见 Felix2yu/.github/docs/makefile.md
#
# 七个必备目标：build / dev / test / lint / fmt / docker / clean
# 其余目标按仓库自身需要添加。
.PHONY: build dev test lint lint-strict fmt docker clean

BIN  := bin/qbhive
MAIN := ./cmd/server

## 产出 $(BIN)。前端是 web/static 下的静态文件，运行时按路径读取，不参与编译
build:
	mkdir -p bin
	go build -trimpath -ldflags "-s -w" -o $(BIN) $(MAIN)

dev:
	go run $(MAIN)

test:
	go test ./...

lint:
	go vet ./...

lint-strict: lint
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else echo "  （未安装 golangci-lint，跳过）"; fi

fmt:
	gofmt -w $$(git ls-files '*.go')

docker: build
	docker build -t qbhive:dev .

clean:
	rm -rf bin
