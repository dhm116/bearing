// Command aws checks that the AWS SDK for Go v2 works inside an Extism guest
// with HTTP routed through bearing_call. Its one export, get_caller_identity,
// calls sts:GetCallerIdentity against the endpoint given as input (a stub
// on the host; no real AWS calls).
//
// Input is JSON {"endpoint": "...", "sign": bool}. With sign, the request is
// SigV4-signed in the guest with dummy static keys, to show the signer works
// in WASM; without it, credentials are anonymous and signing is left to the
// host, which is what ADR 9's "adapter never sees credentials" needs.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/extism/go-pdk"

	_ "bearing.example/spikes/wasm/guest/bearingcall"
	"bearing.example/spikes/wasm/guest/capability"
)

//go:wasmexport get_caller_identity
func getCallerIdentity() int32 {
	var in struct {
		Endpoint string `json:"endpoint"`
		Sign     bool   `json:"sign"`
	}
	if err := json.Unmarshal(pdk.Input(), &in); err != nil {
		pdk.SetError(err)
		return 1
	}
	var creds aws.CredentialsProvider = aws.AnonymousCredentials{}
	if in.Sign {
		creds = credentials.NewStaticCredentialsProvider("AKIDSPIKEDUMMY", "dummy-not-a-secret", "")
	}
	c := sts.New(sts.Options{
		Region:       "us-east-1",
		Credentials:  creds,
		BaseEndpoint: aws.String(in.Endpoint),
		HTTPClient:   &http.Client{Transport: capability.Transport{}},
	})
	out, err := c.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
	if err != nil {
		pdk.SetError(err)
		return 1
	}
	b, _ := json.Marshal(map[string]string{
		"account": aws.ToString(out.Account), "arn": aws.ToString(out.Arn), "user_id": aws.ToString(out.UserId),
	})
	pdk.Output(b)
	return 0
}

func main() {}
