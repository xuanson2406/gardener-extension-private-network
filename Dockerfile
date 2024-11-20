# SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
#
# SPDX-License-Identifier: Apache-2.0

############# builder
FROM golang:1.21.1 AS builder

WORKDIR /go/src/github.com/gardener/gardener-extension-private-network
COPY . .
RUN make install

############# gardener-extension-private-network
FROM alpine:3.15.0 AS gardener-extension-private-network

COPY charts /charts
COPY --from=builder /go/bin/gardener-extension-private-network /gardener-extension-private-network
ENTRYPOINT ["/gardener-extension-private-network"]
