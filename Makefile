HELM_PLUGINS ?= $(shell helm env | awk -F\= '/HELM_PLUGINS/{print $2}' | tr -d \" )


.PHONY: build
build:
	mkdir -p bin/
	go build -i -v -o bin/helmdiff -ldflags="$(LDFLAGS)"

.PHONY: install
install: build
	mkdir -p $(HELM_PLUGINS)/helmdiff/bin
	cp bin/helmdiff $(HELM_PLUGINS)/helmdiff/bin
	cp plugin.yaml $(HELM_PLUGINS)/helmdiff/