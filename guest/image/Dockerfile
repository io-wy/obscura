FROM node:22-bookworm-slim AS builder
RUN apt-get update && apt-get install -y --no-install-recommends protobuf-compiler && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY guest/runner/package*.json guest/runner/
RUN cd guest/runner && npm ci --no-audit --no-fund
COPY guest/runner/tsconfig.json guest/runner/tsconfig.json
COPY guest/runner/src guest/runner/src
COPY api/proto api/proto
RUN mkdir -p guest/runner/src/gen && protoc -I api/proto \
    --plugin=protoc-gen-es=guest/runner/node_modules/.bin/protoc-gen-es \
    --es_out=guest/runner/src/gen --es_opt=target=ts,import_extension=js \
    api/proto/agentruntime/v1/guest.proto
RUN cd guest/runner && npm run build && npm prune --omit=dev

FROM node:22-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends iproute2 socat ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /build/guest/runner/dist /opt/obscura/dist
COPY --from=builder /build/guest/runner/node_modules /opt/obscura/node_modules
COPY --from=builder /build/guest/runner/package.json /opt/obscura/package.json
COPY guest/image/init.sh /sbin/init
RUN chmod 0755 /sbin/init && mkdir -p /work
WORKDIR /opt/obscura
CMD ["/sbin/init"]
