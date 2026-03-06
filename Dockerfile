FROM golang:1.24.6-alpine AS build

WORKDIR /app

RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /api .


FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=build /api /api

EXPOSE 8080
ENV PORT=8080

USER nonroot:nonroot
ENTRYPOINT ["/api"]

