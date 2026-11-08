# gitopsify with the tools it calls: git and ssh (generate --push), helm (diff
# of Helm apps, the plain apply script) and kubectl.
#   docker run --rm -v ~/.kube:/home/gitopsify/.kube:ro -v "$PWD:/work" hayone/gitopsify scan
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev COMMIT="" DATE=""
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The tagged SDK go.mod requires, as go install gets it (not go.work's ./sdk).
ENV GOWORK=off
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/hayone1/gitopsify/internal/cli.Version=$VERSION -X github.com/hayone1/gitopsify/internal/cli.Commit=$COMMIT -X github.com/hayone1/gitopsify/internal/cli.Date=$DATE" \
      -o /out/gitopsify ./cmd/gitopsify

FROM --platform=$BUILDPLATFORM alpine:3.20 AS tools
ARG TARGETARCH HELM_VERSION=v3.19.0 KUBECTL_VERSION=v1.34.1
RUN apk add --no-cache curl \
 && curl -fsSL "https://get.helm.sh/helm-$HELM_VERSION-linux-$TARGETARCH.tar.gz" | tar -xz -C /tmp \
 && mv "/tmp/linux-$TARGETARCH/helm" /usr/local/bin/helm \
 && curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl" \
 && chmod +x /usr/local/bin/kubectl

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git openssh-client \
 && adduser -D -u 10001 gitopsify
COPY --from=tools /usr/local/bin/helm /usr/local/bin/kubectl /usr/local/bin/
COPY --from=build /out/gitopsify /usr/local/bin/gitopsify
USER gitopsify
WORKDIR /work
ENTRYPOINT ["gitopsify"]
CMD ["--help"]
