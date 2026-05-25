APP     := megaconf
VERSION := 2.1
LDFLAGS := -ldflags="-s -w -extldflags=-static"

DIST_DIR := dist

.PHONY: all build clean

build:
	go mod tidy
	go build $(LDFLAGS) -o $(APP) .

all:
	go mod tidy
	mkdir -p $(DIST_DIR)
	CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP) .
	tar -czf $(APP)-$(VERSION)-linux-amd64.tar.gz  -C $(DIST_DIR) $(APP) -C $(CURDIR) devices.db commands
	CGO_ENABLED=0 GOOS=linux  GOARCH=386   go build $(LDFLAGS) -o $(DIST_DIR)/$(APP) .
	tar -czf $(APP)-$(VERSION)-linux-386.tar.gz    -C $(DIST_DIR) $(APP) -C $(CURDIR) devices.db commands
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP) .
	tar -czf $(APP)-$(VERSION)-linux-arm64.tar.gz  -C $(DIST_DIR) $(APP) -C $(CURDIR) devices.db commands
	CGO_ENABLED=0 GOOS=linux  GOARCH=arm   go build $(LDFLAGS) -o $(DIST_DIR)/$(APP) .
	tar -czf $(APP)-$(VERSION)-linux-arm.tar.gz    -C $(DIST_DIR) $(APP) -C $(CURDIR) devices.db commands
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP) .
	tar -czf $(APP)-$(VERSION)-darwin-amd64.tar.gz -C $(DIST_DIR) $(APP) -C $(CURDIR) devices.db commands
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(DIST_DIR)/$(APP) .
	tar -czf $(APP)-$(VERSION)-darwin-arm64.tar.gz -C $(DIST_DIR) $(APP) -C $(CURDIR) devices.db commands
	rm -rf $(DIST_DIR)
	@echo ""
	@ls -lh $(APP)-$(VERSION)-*.tar.gz

clean:
	rm -f $(APP)
	rm -rf $(DIST_DIR)
	rm -f $(APP)-$(VERSION)-*.tar.gz
