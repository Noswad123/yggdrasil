APP_NAME = ygg
CMD_PATH = ./cmd/ygg
BIN_DIR = ./bin
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: all build install check test fmt clean

all: install

build:
	@mkdir -p "$(BIN_DIR)"
	go build -o "$(BIN_DIR)/$(APP_NAME)" "$(CMD_PATH)"

install: build
	@mkdir -p "$(INSTALL_DIR)"
	install -m 0755 "$(BIN_DIR)/$(APP_NAME)" "$(INSTALL_DIR)/$(APP_NAME)"

check:
	go test ./...

test: check

fmt:
	gofmt -w cmd internal

clean:
	rm -f "$(BIN_DIR)/$(APP_NAME)"
