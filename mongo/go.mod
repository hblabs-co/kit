module github.com/hblabs-co/kit/mongo

go 1.26.2

require (
	github.com/joho/godotenv v1.5.1 // indirect
	go.mongodb.org/mongo-driver/v2 v2.6.0
	golang.org/x/crypto v0.50.0 // indirect
)

require (
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.2.0 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.36.0 // indirect
)

require github.com/hblabs-co/kit/core v0.2.0

replace github.com/hblabs-co/kit/core => ../core
