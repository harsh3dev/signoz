# Use the repository root Makefile for all commands.
# Example: cd .. && make autopilot

ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)

.DEFAULT_GOAL := help

.PHONY: help build test vet autopilot-build autopilot-test autopilot-vet

help:
	@$(MAKE) -C $(ROOT) help

build: autopilot-build
test: autopilot-test
vet: autopilot-vet

autopilot-build autopilot-test autopilot-vet:
	@$(MAKE) -C $(ROOT) $@
