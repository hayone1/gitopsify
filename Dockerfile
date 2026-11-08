# gitopsify with the tools it calls: git and ssh (generate --push), helm (diff
# of Helm apps, the plain apply script), kubectl and sops, and the web UI.
#   docker run --rm -v ~/.kube:/home/gitopsify/.kube:ro -v "$PWD:/work" hayone/gitopsify scan
#   docker run -p 8080:8080 -v ./server.yaml:/etc/gitopsify/server.yaml:ro hayone/gitopsify server
# The web UI, built once for every platform (it's static files).
FROM --platform=$BUILDPLATFORM node:26-alpine AS ui
WORKDIR /ui
RUN npm install -g pnpm@10.20.0 >/dev/null
COPY ui/package.json ui/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile --ignore-scripts
COPY ui/ ./
RUN pnpm exec nuxt prepare && pnpm generate

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev COMMIT="" DATE=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /ui/.output/public/ web/dist/
# The tagged SDK go.mod requires, as go install gets it (not go.work's ./sdk).
ENV GOWORK=off
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/hayone1/gitopsify/internal/cli.Version=$VERSION -X github.com/hayone1/gitopsify/internal/cli.Commit=$COMMIT -X github.com/hayone1/gitopsify/internal/cli.Date=$DATE" \
      -o /out/gitopsify ./cmd/gitopsify

FROM --platform=$BUILDPLATFORM alpine:3.20 AS tools
ARG TARGETARCH HELM_VERSION=v3.19.0 KUBECTL_VERSION=v1.34.1
# sops, pinned by checksum (from the release's checksums); SOPS_VERSION=""
# builds without it.
ARG SOPS_VERSION=v3.13.3 \
    SOPS_SHA256_amd64=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b \
    SOPS_SHA256_arm64=53b0abacd38ef1b12a66d6c100956691b9cefce018d91f81e73ddf7438b94d77
# Every download is checked against its project's published checksum.
RUN apk add --no-cache curl \
 && cd /tmp && get() { curl -fsSL --retry 5 --retry-all-errors "$@"; } \
 && get -O "https://get.helm.sh/helm-$HELM_VERSION-linux-$TARGETARCH.tar.gz" \
 && get "https://get.helm.sh/helm-$HELM_VERSION-linux-$TARGETARCH.tar.gz.sha256sum" | sha256sum -c - \
 && tar -xzf "helm-$HELM_VERSION-linux-$TARGETARCH.tar.gz" && mv "linux-$TARGETARCH/helm" /usr/local/bin/helm \
 && get -o /usr/local/bin/kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl" \
 && echo "$(get "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl.sha256")  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod +x /usr/local/bin/kubectl \
 && if [ -n "$SOPS_VERSION" ]; then \
      sum=$(eval echo "\$SOPS_SHA256_$TARGETARCH") \
      && get -o /usr/local/bin/sops "https://github.com/getsops/sops/releases/download/$SOPS_VERSION/sops-$SOPS_VERSION.linux.$TARGETARCH" \
      && echo "$sum  /usr/local/bin/sops" | sha256sum -c - && chmod +x /usr/local/bin/sops; \
    else printf '#!/bin/sh\necho "this image was built without sops" >&2; exit 1\n' > /usr/local/bin/sops && chmod +x /usr/local/bin/sops; fi

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git openssh-client \
 && adduser -D -u 10001 gitopsify \
 && mkdir -p /var/lib/gitopsify /etc/gitopsify && chown gitopsify /var/lib/gitopsify
COPY --from=tools /usr/local/bin/helm /usr/local/bin/kubectl /usr/local/bin/sops /usr/local/bin/
COPY --from=build /out/gitopsify /usr/local/bin/gitopsify
USER gitopsify
WORKDIR /work
# gitopsify server listens here.
EXPOSE 8080
ENTRYPOINT ["gitopsify"]
CMD ["--help"]
