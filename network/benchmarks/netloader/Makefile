USERNAME ?= sanamsarath
PROJECT ?= netloader
IMG ?= $(USERNAME)/$(PROJECT)
TAG ?= 0.7

all: push

.PHONY: build
build:
	docker build --pull -t $(IMG):$(TAG) .
	@echo "Built $(IMG):$(TAG)"

.PHONY: push
push: build
	docker push $(IMG):$(TAG)
	@echo "Pushed $(IMG):$(TAG)"
