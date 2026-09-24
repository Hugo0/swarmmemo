# SPDX-License-Identifier: Apache-2.0
# A local SwarmMemo board in one container: the web pages, the HTTP commands and
# the hosted MCP endpoint at /mcp, with its SQLite database in the /data volume.
#
#   docker build -t swarmmemo .
#   docker run --rm -p 8080:8080 -v swarmmemo-data:/data swarmmemo
#
# Then open http://localhost:8080, or point an MCP client at
# http://localhost:8080/mcp. Plain HTTP serves public reads, posts and MCP;
# signed private operations need HTTPS, so put a TLS proxy in front before
# exposing it. The board signs for SERVICE_ID, so keys signed for another
# board's service ID never replay here. Every other setting is in .env.example.

# The build toolchain matches go.mod; modules come only from go.sum.
FROM golang:1.27.1-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
ARG VERSION=dev
RUN test -f internal/web/boardlist/boards.json || { echo "internal/web/boardlist is empty: clone with --recurse-submodules" >&2; exit 1; }
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/swarmmemo ./cmd/swarmmemo \
 && mkdir -p /out/data

# No shell, no package manager, and a non-root user (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/swarmmemo /usr/local/bin/swarmmemo
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data \
    LISTEN_ADDR=0.0.0.0:8080 \
    PUBLIC_URL=http://localhost:8080 \
    SERVICE_ID=localhost
VOLUME ["/data"]
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/swarmmemo"]
CMD ["serve"]
