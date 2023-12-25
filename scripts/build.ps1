go env -w GOOS=linux
go env -w CGO_ENABLED=0
go build -o ../build ../cmd
go env -w GOOS=windows