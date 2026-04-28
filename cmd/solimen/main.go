package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"downlink/cmd/solimen/internal/api"
	"downlink/pkg/chromium"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

//go:embed extension
var embeddedExtension embed.FS

func init() {
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
		PadLevelText:  true,
	})
	log.SetOutput(os.Stdout)
}

// extractExtension writes the embedded extension files to a new temp directory
// and returns its path. The caller is responsible for removing it when done.
func extractExtension(embedded embed.FS) (string, error) {
	tmpDir, err := os.MkdirTemp("", "solimen-ext-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for extension: %w", err)
	}

	err = fs.WalkDir(embedded, "extension", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := embedded.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tmpDir, d.Name()), data, 0644)
	})
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to extract extension files: %w", err)
	}

	return tmpDir, nil
}

// copyDir copies the flat contents of src into a new temp directory and returns
// the path. The caller is responsible for removing it when done.
func copyDir(src string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "solimen-ext-*")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tmpDir, d.Name()), data, 0644)
	})
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	return tmpDir, nil
}

// prepareExtDirs returns N extension directories and a cleanup function.
//   - N=1 + --ext-dir set: use extDir directly, no-op cleanup
//   - Otherwise: create N temp copies (from embedded or extDir)
func prepareExtDirs(extDirSet bool, extDir string, n int) ([]string, func(), error) {
	if n == 1 && extDirSet {
		return []string{extDir}, func() {}, nil
	}
	dirs := make([]string, n)
	for i := range n {
		var (
			d   string
			err error
		)
		if !extDirSet {
			d, err = extractExtension(embeddedExtension)
		} else {
			d, err = copyDir(extDir)
		}
		if err != nil {
			for j := range i {
				os.RemoveAll(dirs[j])
			}
			return nil, func() {}, fmt.Errorf("prepareExtDirs instance %d: %w", i, err)
		}
		dirs[i] = d
	}
	cleanup := func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}
	return dirs, cleanup, nil
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "solimen",
		Short: "Chromium-backed DOM scraping service",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			viper.SetEnvPrefix("SOLIMEN")
			viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
			viper.AutomaticEnv()
			return viper.BindPFlags(cmd.Flags())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			host := viper.GetString("host")
			port := viper.GetInt("port")
			extDir := viper.GetString("ext-dir")
			instances := viper.GetInt("instances")
			useSandbox := viper.GetBool("use-sandbox")
			noSandbox := !useSandbox

			if instances < 1 {
				return fmt.Errorf("--instances must be >= 1")
			}

			listenAddr := fmt.Sprintf("%s:%d", host, port)

			extDirSet := cmd.Flags().Changed("ext-dir") || os.Getenv("SOLIMEN_EXT_DIR") != ""
			extDirs, cleanup, err := prepareExtDirs(extDirSet, extDir, instances)
			if err != nil {
				return err
			}
			defer cleanup()

			scrapers := make([]*chromium.ChromiumScraper, instances)
			for i := range instances {
				scrapers[i] = chromium.NewChromiumScraper(extDirs[i], i, noSandbox)
			}

			var (
				scraper     chromium.Scraper
				stopScraper func()
			)
			if instances == 1 {
				if err := scrapers[0].Start(); err != nil {
					return fmt.Errorf("failed to start chromium scraper: %w", err)
				}
				scraper = scrapers[0]
				stopScraper = scrapers[0].Stop
			} else {
				pool := chromium.NewChromiumPool(scrapers)
				if err := pool.Start(); err != nil {
					return fmt.Errorf("failed to start chromium pool: %w", err)
				}
				scraper = pool
				stopScraper = pool.Stop
				log.WithField("instances", instances).Info("solimen: chromium pool started")
			}

			srv := api.NewServer(listenAddr, scraper)

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			go func() {
				log.WithField("addr", listenAddr).Info("solimen: HTTP API listening")
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.WithError(err).Fatal("solimen: HTTP server error")
				}
			}()

			<-ctx.Done()
			log.Info("solimen: shutting down")

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.Shutdown(shutdownCtx)
			stopScraper()
			return nil
		},
	}

	rootCmd.Flags().StringP("host", "H", "0.0.0.0", "HTTP listen host")
	rootCmd.Flags().IntP("port", "p", 5011, "HTTP listen port")
	rootCmd.Flags().String("ext-dir", "", "Path to a Chromium extension directory (overrides the embedded extension)")
	rootCmd.Flags().IntP("instances", "n", 1, "Number of parallel Chromium instances")
	rootCmd.Flags().Bool("use-sandbox", false, "Enable Chromium sandbox (disabled by default; required in most container environments)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
