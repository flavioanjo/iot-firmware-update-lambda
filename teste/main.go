package main

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	urlLocal     = "http://localhost:8080/default/DownloadIOTFirmware"
	urlPrimary   = "https://iot-firmware.example.com/update"
	urlSecondary = "https://iot-firmware.example.com/backup/update"
	blockSize    = 1024
	outputFile   = "firmware_download.bin"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run . <device_id> [block_size] [primary|secondary|<url>]")
		fmt.Println("")
		fmt.Println("  device_id    - Device identifier (required)")
		fmt.Println("  block_size   - Firmware block size in bytes (optional, default 1024)")
		fmt.Println("  primary      - Use the primary firmware server endpoint")
		fmt.Println("  secondary    - Use the secondary firmware server endpoint")
		fmt.Println("  <url>        - Use a custom endpoint (http:// or https://)")
		fmt.Println("")
		fmt.Println("Examples:")
		fmt.Println("  go run . 12345                 (local, 1024-byte block)")
		fmt.Println("  go run . 12345 2048            (local, 2048-byte block)")
		fmt.Println("  go run . 12345 1024 primary    (primary firmware server, 1024-byte block)")
		fmt.Println("  go run . 12345 1024 secondary  (secondary firmware server, 1024-byte block)")
		fmt.Println("  go run . 12345 1024 http://localhost:9090/default/DownloadIOTFirmware")
		os.Exit(1)
	}

	deviceID := os.Args[1]

	// The block size can be passed as the second argument (optional)
	bloco := blockSize
	if len(os.Args) >= 3 && !strings.Contains(os.Args[2], "://") && os.Args[2] != "primary" && os.Args[2] != "secondary" {
		val, err := strconv.Atoi(os.Args[2])
		if err != nil || val <= 0 {
			fmt.Printf("Invalid block size %q: must be a positive integer\n", os.Args[2])
			os.Exit(1)
		}
		bloco = val
	}

	// Select the endpoint: local, primary, or secondary
	baseURL := urlLocal
	for _, arg := range os.Args[1:] {
		if arg == "primary" {
			baseURL = urlPrimary
			break
		}
		if arg == "secondary" {
			baseURL = urlSecondary
			break
		}
		if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
			baseURL = arg
			break
		}
	}

	fmt.Println("==================================================")
	fmt.Printf("  Firmware download test\n")
	fmt.Printf("  Device: %s\n", deviceID)
	fmt.Printf("  Block size: %d bytes\n", bloco)
	fmt.Printf("  Server: %s\n", baseURL)
	fmt.Println("==================================================")
	fmt.Println()

	client := &http.Client{Timeout: 30 * time.Second}

	// ============================================================
	// STEP 0: Health check
	// ============================================================
	fmt.Print("[Health] Checking firmware server... ")

	// Build the health URL using the same endpoint without parameters
	healthURL := baseURL
	respHealth, err := client.Get(healthURL)
	if err != nil {
		fmt.Printf("Failed (%v)\n", err)
		os.Exit(1)
	}
	respHealth.Body.Close()
	if respHealth.StatusCode == 200 {
		fmt.Println("Healthy")
	} else {
		fmt.Printf("Failed (HTTP %d)\n", respHealth.StatusCode)
		os.Exit(1)
	}
	fmt.Println()

	// ============================================================
	// STEP 1: Request with offset=-1 to fetch firmware metadata
	// ============================================================
	fmt.Println("[Meta] Requesting firmware metadata (offset=-1)...")

	urlMeta := fmt.Sprintf("%s?device_id=%s&offset=-1", baseURL, url.QueryEscape(deviceID))
	resp, err := client.Get(urlMeta)
	if err != nil {
		fmt.Printf("Firmware metadata request failed: %v\n", err)
		os.Exit(1)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		fmt.Printf("Firmware metadata read failed: %v\n", err)
		os.Exit(1)
	}

	if resp.StatusCode == 404 {
		fmt.Printf("No pending firmware for device %s\n", deviceID)
		os.Exit(0)
	}
	if resp.StatusCode != 200 {
		fmt.Printf("HTTP %d error: %s\n", resp.StatusCode, string(body))
		os.Exit(1)
	}

	if len(body) != 20 {
		fmt.Printf("Unexpected firmware metadata response length: %d bytes (expected 20)\n", len(body))
		os.Exit(1)
	}

	// Decode: 4 bytes int32 (size) + 16 bytes (raw MD5)
	fmt.Printf("  Received packet (hex): %X\n", body)
	tamanhoFirmware := int(binary.LittleEndian.Uint32(body[0:4]))
	serverMD5 := strings.ToUpper(hex.EncodeToString(body[4:20]))

	fmt.Printf("  Firmware size: %d bytes\n", tamanhoFirmware)
	fmt.Printf("  Server MD5:           %s\n", serverMD5)
	fmt.Printf("  Required blocks:      %d\n", (tamanhoFirmware+bloco-1)/bloco)
	fmt.Println()

	// ============================================================
	// STEP 2: Download firmware block by block
	// ============================================================
	arquivo, err := os.Create(outputFile)
	if err != nil {
		fmt.Printf("Error creating output file: %v\n", err)
		os.Exit(1)
	}
	defer arquivo.Close()

	offset := 0
	totalBaixado := 0
	numRequisicoes := 0
	inicio := time.Now()
	md5Hash := md5.New()

	for {
		chunkURL := fmt.Sprintf("%s?device_id=%s&offset=%d&block_size=%d", baseURL, url.QueryEscape(deviceID), offset, bloco)

		// Safety net: the server must answer with the EOF marker once offset
		// reaches the firmware size, so never request more than one block past it.
		if offset > tamanhoFirmware+bloco {
			fmt.Println("\nServer did not signal end of file; aborting.")
			os.Exit(1)
		}

		numRequisicoes++
		fmt.Printf("[Req #%d] offset=%d, length=%d ... ", numRequisicoes, offset, bloco)

		resp, err := client.Get(chunkURL)
		if err != nil {
			fmt.Printf("\nFirmware request failed: %v\n", err)
			os.Exit(1)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fmt.Printf("\nFirmware response read failed: %v\n", err)
			os.Exit(1)
		}

		if resp.StatusCode != 200 {
			fmt.Printf("\nHTTP %d error: %s\n", resp.StatusCode, string(body))
			os.Remove(outputFile)
			os.Exit(1)
		}

		// The end of the transfer is a text/plain body starting with "*** MD5".
		// Checking the content type avoids mistaking a binary chunk that
		// happens to start with those bytes for the end marker.
		bodyStr := string(body)
		isText := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain")
		if isText && strings.HasPrefix(bodyStr, "*** MD5") {
			fmt.Println("END!")
			break
		}

		// Write the block to the output file and update the MD5 hash
		md5Hash.Write(body)
		bytesEscritos, err := arquivo.Write(body)
		if err != nil {
			fmt.Printf("\nError writing to output file: %v\n", err)
			os.Exit(1)
		}

		totalBaixado += bytesEscritos
		progresso := float64(totalBaixado) / float64(tamanhoFirmware) * 100
		fmt.Printf("OK (%d bytes, total: %d/%d bytes, %.1f%%)\n", bytesEscritos, totalBaixado, tamanhoFirmware, progresso)

		offset += bloco
	}

	duracao := time.Since(inicio)
	localMD5 := fmt.Sprintf("%X", md5Hash.Sum(nil))

	fmt.Println()
	fmt.Println("==================================================")
	fmt.Println("  Firmware download completed!")
	fmt.Printf("  File saved: %s\n", outputFile)
	fmt.Printf("  Total size: %d bytes\n", totalBaixado)
	fmt.Printf("  Requests: %d\n", numRequisicoes)
	fmt.Printf("  Total time: %s\n", duracao.Round(time.Millisecond))
	if duracao.Seconds() > 0 {
		fmt.Printf("  Throughput: %.2f KB/s\n", float64(totalBaixado)/1024/duracao.Seconds())
	}
	fmt.Println("--------------------------------------------------")
	fmt.Printf("  Server MD5: %s\n", serverMD5)
	fmt.Printf("  Local MD5:  %s\n", localMD5)
	if strings.EqualFold(localMD5, serverMD5) {
		fmt.Println("  Result:     Success - MD5 matches")
	} else {
		fmt.Println("  Result:     Error - MD5 mismatch")
		fmt.Println("==================================================")
		arquivo.Close()
		os.Exit(1)
	}
	fmt.Println("==================================================")
}
