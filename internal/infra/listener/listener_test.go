package listener

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTLSModes(t *testing.T) {
	source := httptest.NewTLSServer(nil)
	t.Cleanup(source.Close)
	pair := source.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600))

	for _, mode := range []Mode{StartTLSMode, ImplicitMode} {
		t.Run(string(mode), func(t *testing.T) {
			listener, err := New(t.Context(), Config{Addr: "127.0.0.1:0", TLS: TLS{Mode: mode, CertFile: certFile, KeyFile: keyFile}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, listener.Close()) })
			require.Len(t, listener.TLSConfig().Certificates, 1)

			accepted := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					accepted <- err
					return
				}
				defer func() { _ = conn.Close() }()
				tlsConn, isTLS := conn.(*tls.Conn)
				if isTLS != (mode == ImplicitMode) {
					accepted <- errors.New("listener TLS mode does not match")
					return
				}
				if isTLS {
					accepted <- tlsConn.HandshakeContext(t.Context())
					return
				}
				accepted <- nil
			}()
			if mode == ImplicitMode {
				roots := x509.NewCertPool()
				roots.AddCert(source.Certificate())
				dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: time.Second}, Config: &tls.Config{RootCAs: roots}}
				conn, err := dialer.DialContext(t.Context(), "tcp", listener.Addr().String())
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			} else {
				conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", listener.Addr().String())
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			}
			require.NoError(t, <-accepted)
		})
	}
}
