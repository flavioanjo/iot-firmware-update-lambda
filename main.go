package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http" // Required for the local firmware server
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	// The blank identifier (_) imports the package only so the SQL Server
	// driver registers itself with database/sql; it is not called directly.
	_ "github.com/microsoft/go-mssqldb"
)

// =======================================================
// LOCAL ENVIRONMENT AND LOCAL SERVER CONTROL
// =======================================================

func main() {
	// If this variable is set locally, the service runs as a local HTTP server
	if os.Getenv("LOCAL_ENVIRONMENT") == "true" {
		startLocalServer()
	} else {
		// In the cloud environment this variable is not set, so the Lambda runs natively
		lambda.Start(HandleRequest)
	}
}

// This function runs only in local mode to simulate the API Gateway behavior
func startLocalServer() {
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})

	http.HandleFunc("/default/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("OK"))
	})

	http.HandleFunc("/default/DownloadIOTFirmware", func(w http.ResponseWriter, r *http.Request) {

		// 1. Simulate the request object created by the API gateway
		fakeRequest := events.APIGatewayProxyRequest{
			QueryStringParameters: map[string]string{
				"device_id":  r.URL.Query().Get("device_id"),
				"offset":     r.URL.Query().Get("offset"),
				"block_size": r.URL.Query().Get("block_size"),
			},
		}

		// 2. Invoke the existing business logic without changing behavior
		response, _ := HandleRequest(context.Background(), fakeRequest)

		// 3. Forward headers to the HTTP client
		for key, value := range response.Headers {
			w.Header().Set(key, value)
		}
		w.WriteHeader(response.StatusCode)

		// 4. Decode the base64 payload back to binary data for local testing
		if response.IsBase64Encoded {
			binaryData, _ := base64.StdEncoding.DecodeString(response.Body)
			w.Write(binaryData)
		} else {
			w.Write([]byte(response.Body))
		}
	})

	fmt.Println("==================================================")
	fmt.Println("   Firmware server running on port 8080...")
	fmt.Println("   Active route: http://localhost:8080/default/DownloadIOTFirmware")
	fmt.Println("==================================================")

	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		fmt.Println("Firmware server startup failed:", err)
	}
}
