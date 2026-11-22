package main

import (
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	// The blank identifier (_) imports the package only so the SQL Server
	// driver registers itself with database/sql; it is not called directly.
	_ "github.com/microsoft/go-mssqldb"
)

func main() {
	// LOCAL_ENVIRONMENT=true runs the service as a plain HTTP server, which
	// mimics the API Gateway integration for development and testing.
	if os.Getenv("LOCAL_ENVIRONMENT") == "true" {
		if err := runLocalServer(); err != nil {
			newLogger().Error("local server stopped", "error", err.Error())
			os.Exit(1)
		}
		return
	}
	lambda.Start(HandleRequest)
}
