# The demo image: bearing, its three adapters and the Acme fakes, for
# compose.yaml and docs/demo.md. Bearing needs CGO (DuckDB), so the binaries
# are built against glibc and run on a base image that has it and libstdc++.
# Base images are pinned by digest; update them with
# `docker buildx imagetools inspect <image>`.

FROM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/ \
      ./cmd/bearing ./cmd/bearing-adapter-github ./cmd/bearing-adapter-kubernetes \
      ./cmd/bearing-adapter-authentik ./cmd/bearing-demo-fakes
# The base image has no shell: make the directories the service user owns here.
RUN mkdir -p /rootfs/var/lib/bearing /rootfs/run/bearing /rootfs/certs \
 && chmod 0755 /rootfs/var/lib/bearing /rootfs/certs && chmod 0700 /rootfs/run/bearing
# Settings and declarations, owned by root and not writable by anyone else,
# as boot.Load requires. Laid out here because COPY --chmod would also set
# the mode of the directories it creates, leaving them untraversable.
RUN mkdir -p /etcfs/bearing/declarations \
 && cp demo/settings.json /etcfs/bearing/settings.json \
 && cp testdata/declarations/*.json /etcfs/bearing/declarations/ \
 && find /etcfs -type d -exec chmod 0755 {} + && find /etcfs -type f -exec chmod 0644 {} +

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:9dac0a79194e45a7da0158a9c6da57b217585af0786db3845d1f0ec1a0dd182f
COPY --from=build /out/ /usr/local/bin/
COPY --from=build /etcfs/ /etc/
# The store, the API socket (private to the service user) and the demo
# certificates belong to the service user (65532).
COPY --from=build --chown=65532:65532 /rootfs/var/lib/bearing /var/lib/bearing
COPY --from=build --chown=65532:65532 /rootfs/run/bearing /run/bearing
COPY --from=build --chown=65532:65532 /rootfs/certs /certs
USER 65532:65532
EXPOSE 8443
CMD ["/usr/local/bin/bearing", "server", "--settings", "/etc/bearing/settings.json"]
