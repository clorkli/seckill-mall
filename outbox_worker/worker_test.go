package main

import "testing"

func TestParseOrderMessage(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
		want    OrderMessage
	}{
		{
			name:    "valid order message",
			payload: `{"order_id":"order-1","user_id":1001,"product_id":1,"count":2,"amount":199.5}`,
			want: OrderMessage{
				OrderID:   "order-1",
				UserID:    1001,
				ProductID: 1,
				Count:     2,
				Amount:    199.5,
			},
		},
		{
			name:    "invalid json",
			payload: `{`,
			wantErr: true,
		},
		{
			name:    "missing order id",
			payload: `{"user_id":1001,"product_id":1,"count":2,"amount":199.5}`,
			wantErr: true,
		},
		{
			name:    "invalid user id",
			payload: `{"order_id":"order-1","user_id":0,"product_id":1,"count":2,"amount":199.5}`,
			wantErr: true,
		},
		{
			name:    "invalid product id",
			payload: `{"order_id":"order-1","user_id":1001,"product_id":0,"count":2,"amount":199.5}`,
			wantErr: true,
		},
		{
			name:    "invalid count",
			payload: `{"order_id":"order-1","user_id":1001,"product_id":1,"count":0,"amount":199.5}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOrderMessage(tt.payload)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
