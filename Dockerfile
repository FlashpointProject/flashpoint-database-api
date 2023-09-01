FROM golang:1.20

WORKDIR /app

COPY . .

RUN go mod download
RUN go build -o /app/fpdb *.go

CMD ["/app/fpdb"]
