# Copyright 2017 The Kubernetes Authors.
# #
# # Licensed under the Apache License, Version 2.0 (the "License");
# # you may not use this file except in compliance with the License.
# # You may obtain a copy of the License at
# #
# #     http://www.apache.org/licenses/LICENSE-2.0
# #
# # Unless required by applicable law or agreed to in writing, software
# # distributed under the License is distributed on an "AS IS" BASIS,
# # WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# # See the License for the specific language governing permissions and
# # limitations under the License.

.PHONY: test

# VERSION is based on a date stamp plus the last commit
VERSION?=v$(shell date +%Y%m%d)-$(shell git describe --tags --match "v*")
BRANCH?=$(shell git branch --show-current)
SHA1?=$(shell git rev-parse HEAD)
BUILD=$(shell date +%FT%T%z)
LDFLAG_LOCATION=sigs.k8s.io/descheduler/pkg/version
ARCHS = amd64

GOLANGCI_VERSION := v1.43.0
HAS_GOLANGCI := $(shell ls _output/bin/golangci-lint 2> /dev/null)

# IMAGE is the image name of descheduler
IMAGE:=descheduler:$(VERSION)

build.amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o _output/bin/descheduler sigs.k8s.io/descheduler/cmd/descheduler

image.amd64:
	docker build --build-arg VERSION="$(VERSION)" --build-arg ARCH="amd64" -t $(IMAGE)-amd64 .
