USERNAME ?= karanadive
PROJECT ?= h2playground
REGISTRY ?= acnpublic.azurecr.io
IMG ?= $(USERNAME)/$(PROJECT)
TAG ?= 0.3

all: push

.PHONY: build
build:
	docker build --pull -t $(REGISTRY)/$(IMG):$(TAG) .
	@echo "Built $(IMG):$(TAG)"

.PHONY: push
push: build
	docker push $(REGISTRY)/$(IMG):$(TAG)
	@echo "Pushed $(IMG):$(TAG)"
