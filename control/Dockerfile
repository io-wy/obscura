FROM golang:1.26 AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY control/go.mod control/go.sum ./control/
WORKDIR /src/control
RUN go mod download
WORKDIR /src
COPY control/ ./control/
COPY api/ ./api/
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
COPY scripts/generate-go-proto.sh ./scripts/generate-go-proto.sh
RUN ./scripts/generate-go-proto.sh
WORKDIR /src/control
RUN CGO_ENABLED=0 go build -trimpath -o /out/controller ./cmd/controller && CGO_ENABLED=0 go build -trimpath -o /out/gateway ./cmd/gateway && CGO_ENABLED=0 go build -trimpath -o /out/devruntime ./cmd/devruntime

# Optional Compose test target. The production stage below stays minimal.
FROM build AS control-tests
WORKDIR /src
COPY deploy/ ./deploy/
COPY Makefile ./Makefile
COPY scripts/envtest-assets.sh scripts/test-controller-envtest.sh scripts/generate-go-proto.sh ./scripts/
RUN chmod +x ./scripts/envtest-assets.sh ./scripts/test-controller-envtest.sh ./scripts/generate-go-proto.sh
CMD ["make", "test-controlplane"]

FROM gcr.io/distroless/static-debian12:nonroot AS devruntime
COPY --from=build /out/devruntime /usr/local/bin/devruntime
ENTRYPOINT ["/usr/local/bin/devruntime"]

FROM gcr.io/distroless/static-debian12:nonroot AS production
COPY --from=build /out/controller /usr/local/bin/controller
COPY --from=build /out/gateway /usr/local/bin/gateway
ENTRYPOINT ["/usr/local/bin/controller"]
