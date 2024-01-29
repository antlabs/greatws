package greatws

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpgradeInner(t *testing.T) {
	tests := []struct {
		name    string
		conf    *Config
		wantErr bool
	}{
		{
			name:    "Test with nil EventLoop",
			conf:    &Config{},
			wantErr: true,
		},
		// Add more test cases here
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "/", nil)
			if err != nil {
				t.Fatal(err)
			}

			rr := httptest.NewRecorder()
			_, err = upgradeInner(rr, req, tt.conf)

			if (err != nil) != tt.wantErr {
				t.Errorf("upgradeInner() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
