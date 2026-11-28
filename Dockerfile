ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=alpine:3.23

FROM ${GO_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath \
  -ldflags="-s -w -X gitlab.com/banyan-vcs/git-cdn/internal/buildinfo.Version=${VERSION}" \
  -o /out/git-cdn ./cmd/git-cdn

FROM ${RUNTIME_IMAGE}
RUN apk add --no-cache ca-certificates git openssh-client openssh-keygen \
 && addgroup -g 10001 gitcdn \
 && adduser -D -u 10001 -G gitcdn -h /var/lib/git-cdn gitcdn \
 && mkdir -p /var/lib/git-cdn/caches /etc/git-cdn \
 && chown -R gitcdn:gitcdn /var/lib/git-cdn
COPY --from=build /out/git-cdn /usr/local/bin/git-cdn
COPY docker/config.yaml /etc/git-cdn/config.yaml
COPY docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY LICENSE /usr/share/licenses/git-cdn/LICENSE
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh
USER gitcdn
WORKDIR /var/lib/git-cdn
VOLUME /var/lib/git-cdn
EXPOSE 8080 2222
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["git-cdn", "serve", "--config", "/etc/git-cdn/config.yaml"]
