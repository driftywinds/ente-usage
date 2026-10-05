FROM golang:1 AS build
WORKDIR /src
COPY main.go .
RUN go mod init ente-usage && go mod tidy && CGO_ENABLED=0 go build -o /ente-usage .

FROM gcr.io/distroless/static
COPY --from=build /ente-usage /ente-usage
CMD ["/ente-usage"]