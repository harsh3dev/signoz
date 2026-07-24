FROM node:20-alpine AS ui
WORKDIR /ui
COPY internal/controller/ui/package.json internal/controller/ui/package-lock.json ./
RUN npm ci
COPY internal/controller/ui/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /ui/dist ./internal/controller/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -o /out/autopilot ./cmd/autopilot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/autopilot /autopilot
USER nonroot:nonroot
ENTRYPOINT ["/autopilot"]
