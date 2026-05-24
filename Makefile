APP     := megaconf
VERSION := 2.0
LDFLAGS := -ldflags="-s -w -extldflags=-static"

# целевые платформы: GOOS/GOARCH
TARGETS := \
	linux/amd64 \
	linux/386 \
	linux/arm64 \
	linux/arm \
	freebsd/amd64 \
	freebsd/386 \
	freebsd/arm64 \
	darwin/amd64 \
	darwin/arm64 \
	windows/amd64 \
	windows/386 \
	windows/arm64

DIST_DIR := dist
TGZ      := $(APP)-$(VERSION).tar.gz

.PHONY: all build clean tgz

## build — собирает бинарь для текущей платформы
build:
	go build $(LDFLAGS) -o $(APP) .

## tgz — собирает бинари для всех платформ и упаковывает в архив
tgz:
	mkdir -p $(DIST_DIR)
	$(foreach TARGET,$(TARGETS), \
		$(eval GOOS   := $(word 1,$(subst /, ,$(TARGET)))) \
		$(eval GOARCH := $(word 2,$(subst /, ,$(TARGET)))) \
		$(eval EXT    := $(if $(filter windows,$(GOOS)),.exe,)) \
		CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
			go build $(LDFLAGS) \
			-o $(DIST_DIR)/$(APP)-$(VERSION)-$(GOOS)-$(GOARCH)$(EXT) . ; \
	)
	cp devices.db commands $(DIST_DIR)/
	tar -czf $(TGZ) -C $(DIST_DIR) .
	@echo ""
	@echo "Package ready: $(TGZ)"
	@ls -lh $(TGZ)

## all — бинари для всех платформ + tgz
all: tgz

## clean — удаляет артефакты сборки
clean:
	rm -f $(APP)
	rm -rf $(DIST_DIR)
	rm -f $(TGZ)
