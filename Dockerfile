# Build context is the repository root.
#
# The bot reads MM_MATCHING_REPO_PATH / MM_RISK_CORE_REPO_PATH only as a fallback,
# to discover contract addresses when they are not configured. The task definition
# sets MM_MATCHING_ADDRESS, MM_TRADE_MODULE_ADDRESS and MM_SUBACCOUNTS_ADDRESS
# explicitly, so that fallback never fires and no sibling repo belongs in the image.

FROM golang:1.24-bookworm AS build

WORKDIR /src

# Dependencies first so a source-only change does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG GIT_SHA=unknown

# CGO off and a static build so the result runs on distroless/static.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mm-bot ./cmd/mm-bot

# --------------------------------------------------------------------- runtime

FROM gcr.io/distroless/static-debian12:nonroot

ARG GIT_SHA=unknown
LABEL org.opencontainers.image.revision="${GIT_SHA}"
LABEL org.opencontainers.image.source="https://github.com/numofx/market-maker"

COPY --from=build /out/mm-bot /app/mm-bot

# MM_STATE_FILE defaults under /tmp, which is writable and ephemeral on Fargate.
# The bot rebuilds its state from the book on startup, so losing it costs a cycle,
# not correctness — which is why this needs no volume.
USER nonroot:nonroot

ENTRYPOINT ["/app/mm-bot"]
