APP     := megaconf
VERSION := 2.0
LDFLAGS := -ldflags="-s -w -extldflags=-static"

DIST_DIR := dist

.PHONY: all build clean

build:
	go build $(LDFLAGS) -o $(APP) .

all:
	@mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP)-$(VERSION)-linux-amd64   .
	CGO_ENABLED=0 GOOS=linux   GOARCH=386   go build $(LDFLAGS) -o $(DIST_DIR)/$(APP)-$(VERSION)-linux-386     .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP)-$(VERSION)-linux-arm64   .
	CGO_ENABLED=0 GOOS=linux   GOARCH=arm   go build $(LDFLAGS) -o $(DIST_DIR)/$(APP)-$(VERSION)-linux-arm     .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP)-$(VERSION)-darwin-amd64  .
	CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP)-$(VERSION)-darwin-arm64  .
	tar -czf $(APP)-$(VERSION)-linux-amd64.tar.gz   -C $(DIST_DIR) $(APP)-$(VERSION)-linux-amd64
	tar -czf $(APP)-$(VERSION)-linux-386.tar.gz     -C $(DIST_DIR) $(APP)-$(VERSION)-linux-386
	tar -czf $(APP)-$(VERSION)-linux-arm64.tar.gz   -C $(DIST_DIR) $(APP)-$(VERSION)-linux-arm64
	tar -czf $(APP)-$(VERSION)-linux-arm.tar.gz     -C $(DIST_DIR) $(APP)-$(VERSION)-linux-arm
	tar -czf $(APP)-$(VERSION)-darwin-amd64.tar.gz  -C $(DIST_DIR) $(APP)-$(VERSION)-darwin-amd64
	tar -czf $(APP)-$(VERSION)-darwin-arm64.tar.gz  -C $(DIST_DIR) $(APP)-$(VERSION)-darwin-arm64
	@echo ''
	@ls -lh $(APP)-$(VERSION)-*.tar.gz

clean:
	rm -f $(APP)
	rm -rf $(DIST_DIR)
	rm -f $(APP)-$(VERSION)-*.tar.gz
