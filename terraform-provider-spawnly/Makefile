# terraform-provider-spawnly — local dev build & install.
#
# This module is intentionally OUTSIDE the repo's go.work workspace (its
# Terraform deps shouldn't bleed into the platform), so every go command here
# pins GOWORK=off.
export GOWORK := off

BINARY      := terraform-provider-spawnly
BIN_DIR     := $(CURDIR)/bin
SOURCE_ADDR := registry.terraform.io/spawnly/spawnly
TFRC        := $(CURDIR)/dev.tfrc

.PHONY: build install tfrc test testacc fmt vet clean

## build: compile the provider binary into ./bin
build:
	go build -o $(BIN_DIR)/$(BINARY) .

## tfrc: write a dev_overrides CLI config pointing Terraform at ./bin
tfrc:
	@printf 'provider_installation {\n  dev_overrides {\n    "%s" = "%s"\n  }\n  direct {}\n}\n' "$(SOURCE_ADDR)" "$(BIN_DIR)" > $(TFRC)
	@echo "Wrote $(TFRC)"

## install: build + write dev.tfrc, then print the one env var to export
install: build tfrc
	@echo ""
	@echo "Provider installed at $(BIN_DIR)/$(BINARY)"
	@echo "Point Terraform at it:  export TF_CLI_CONFIG_FILE=$(TFRC)"
	@echo "(Under dev_overrides, skip 'terraform init' — just run plan/apply.)"

## test: unit tests (mock-backed, no cluster needed)
test:
	go test ./...

## testacc: acceptance tests against a live registry (needs TF_ACC + SPAWNLY_*)
testacc:
	TF_ACC=1 go test ./internal/provider/ -v -timeout 20m

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -rf $(BIN_DIR) $(TFRC)
