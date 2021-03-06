// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	firebase "firebase.google.com/go"
	"github.com/gofrs/uuid"
	"github.com/gorilla/mux"
	grpcgw "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama/v3/apigrpc"
	"go.uber.org/zap"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	authTokenInvalidBytes    = []byte(`{"error":"Auth token invalid","message":"Auth token invalid","code":16}`)
	httpKeyInvalidBytes      = []byte(`{"error":"HTTP key invalid","message":"HTTP key invalid","code":16}`)
	noAuthBytes              = []byte(`{"error":"1Auth token or HTTP key required","message":"Auth token or HTTP key required","code":16}`)
	rpcIDMustBeSetBytes      = []byte(`{"error":"RPC ID must be set","message":"RPC ID must be set","code":3}`)
	rpcFunctionNotFoundBytes = []byte(`{"error":"RPC function not found","message":"RPC function not found","code":5}`)
	internalServerErrorBytes = []byte(`{"error":"Internal Server Error","message":"Internal Server Error","code":13}`)
	badJSONBytes             = []byte(`{"error":"json: cannot unmarshal object into Go value of type string","message":"json: cannot unmarshal object into Go value of type string","code":3}`)
	requestBodyTooLargeBytes = []byte(`{"code":3, "message":"http: request body too large"}`)
)

func (s *ApiServer) RpcFuncHttp(w http.ResponseWriter, r *http.Request) {
	// Check first token then HTTP key for authentication, and add user info to the context.
	queryParams := r.URL.Query()
	var tokenAuth bool
	var userID uuid.UUID
	var username string
	var vars map[string]string
	var expiry int64

	if auth := r.Header["Authorization"]; len(auth) >= 1 {
		ctx, opt := context.Background(), option.WithCredentialsFile("/nakama/data/modules/service-account.json")
		app, err := firebase.NewApp(ctx, nil, opt)
		if err != nil {
			s.logger.Debug("error initializing app: %v\n", zap.Error(err))
			return
		}

		s.logger.Debug("Firebase admin ready")

		client, err := app.Auth(ctx)
		if err != nil {
			s.logger.Error("error getting Auth client: %v\n", zap.Error(err))
			return
		}

		const prefix = "Bearer "
		var idToken = auth[0]
		if !strings.HasPrefix(idToken, prefix) {
			s.logger.Error("error getting ID token: %v\n", zap.Error(err))
			return
		}
		idToken = idToken[len(prefix):]

		s.logger.Debug(idToken, zap.Error(err))
		firebaseIDToken, err := client.VerifyIDToken(ctx, idToken)
		if err != nil {
			s.logger.Error("error verifying ID token: %v\n", zap.Error(err))
			// Auth token not valid or expired.
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, err := w.Write(authTokenInvalidBytes)
			if err != nil {
				s.logger.Debug("Error writing response to client", zap.Error(err))
			}

			return
		}

		s.logger.Debug("Verified ID token:")
		s.logger.Debug(firebaseIDToken.UID)
		// s.logger.Debug("session.Token", zap.Error(err))
		// s.logger.Debug(session.Token, zap.Error(err))

		// outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
		// 	"authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("defaultkey:")),
		// }))

		// session, err := s.AuthenticateCustom(outgoingCtx, &api.AuthenticateCustomRequest{
		// 	Account: &api.AccountCustom{
		// 		Id: firebaseIDToken.UID,
		// 	},
		// 	Username: firebaseIDToken.UID,
		// })

		// if err != nil {
		// 	s.logger.Error("error verifying ID token: %v\n", zap.Error(err))
		// 	// Auth token not valid or expired.
		// 	w.Header().Set("content-type", "application/json")
		// 	w.WriteHeader(http.StatusUnauthorized)
		// 	_, err := w.Write(authTokenInvalidBytes)
		// 	if err != nil {
		// 		s.logger.Debug("Error writing response to client", zap.Error(err))
		// 	}

		// 	return
		// }

		// session, err := s.AuthenticateCustom(ctx, "48656C6C6F20776F726C64", "username", true)
		// if err != nil {
		// 	s.logger.Debug("Session error", zap.Error(err))
		// }

		outgoingCtx := metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
			"authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("defaultkey:")),
		}))
		conn, err := grpc.DialContext(outgoingCtx, "localhost:7349", grpc.WithInsecure())
		if err != nil {
			s.logger.Debug("Error writing response to client", zap.Error(err))
			return
		}

		apiclient := apigrpc.NewNakamaClient(conn)
		session, err := apiclient.AuthenticateCustom(outgoingCtx, &api.AuthenticateCustomRequest{
			Account: &api.AccountCustom{
				Id: firebaseIDToken.UID,
			},
			// Username: GenerateString(),
			Username: firebaseIDToken.UID,
		})
		if err != nil {
			s.logger.Debug("Error writing response to client", zap.Error(err))
			return
		}

		s.logger.Debug(session.Token)
		s.logger.Debug(session.RefreshToken)
		s.logger.Debug(session.GetRefreshToken())
		// userID, username, vars, err := AuthenticateCustom(context.Background(), logger, db, uuid.Must(uuid.NewV4()).String(), uuid.Must(uuid.NewV4()).String(), true)
		// userID, username, vars, expiry, tokenAuth = parseBearerAuth([]byte(s.config.GetSession().EncryptionKey), session.Token)
		userID, username, vars, expiry, tokenAuth = parseToken([]byte(s.config.GetSession().RefreshEncryptionKey), session.GetRefreshToken())
		if !tokenAuth {
			// Auth token not valid or expired.
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, err := w.Write(authTokenInvalidBytes)
			if err != nil {
				s.logger.Debug("Error writing response to client", zap.Error(err))
			}
			return
		}

	} else if httpKey := queryParams.Get("http_key"); httpKey != "" {
		if httpKey != s.config.GetRuntime().HTTPKey {
			// HTTP key did not match.
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, err := w.Write(httpKeyInvalidBytes)
			if err != nil {
				s.logger.Debug("Error writing response to client", zap.Error(err))
			}
			return
		}
	} else {
		// No authentication present.
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, err := w.Write(noAuthBytes)
		if err != nil {
			s.logger.Debug("Error writing response to client", zap.Error(err))
		}
		return
	}

	// After this point the RPC will be captured in metrics.
	start := time.Now()
	var success bool
	var recvBytes, sentBytes int
	defer func() {
		s.metrics.Api("Rpc", time.Since(start), int64(recvBytes), int64(sentBytes), !success)
	}()
	var err error

	// Check the RPC function ID.
	maybeID, ok := mux.Vars(r)["id"]
	if !ok || maybeID == "" {
		// Missing RPC function ID.
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		sentBytes, err = w.Write(rpcIDMustBeSetBytes)
		if err != nil {
			s.logger.Debug("Error writing response to client", zap.Error(err))
		}
		return
	}
	id := strings.ToLower(maybeID)

	// Find the correct RPC function.
	fn := s.runtime.Rpc(id)
	if fn == nil {
		// No function registered for this ID.
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		sentBytes, err = w.Write(rpcFunctionNotFoundBytes)
		if err != nil {
			s.logger.Debug("Error writing response to client", zap.Error(err))
		}
		return
	}

	// Check if we need to mimic existing GRPC Gateway behaviour or expect to receive/send unwrapped data.
	// Any value for this query parameter, including the parameter existing with an empty value, will
	// indicate that raw behaviour is expected.
	_, unwrap := queryParams["unwrap"]

	// Prepare input to function.
	var payload string
	if r.Method == "POST" {
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			// Request body too large.
			if err.Error() == "http: request body too large" {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				sentBytes, err = w.Write(requestBodyTooLargeBytes)
				if err != nil {
					s.logger.Debug("Error writing response to client", zap.Error(err))
				}
				return
			}

			// Other error reading request body.
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			sentBytes, err = w.Write(internalServerErrorBytes)
			if err != nil {
				s.logger.Debug("Error writing response to client", zap.Error(err))
			}
			return
		}
		recvBytes = len(b)

		// Maybe attempt to decode to a JSON string to mimic existing GRPC Gateway behaviour.
		if !unwrap {
			err = json.Unmarshal(b, &payload)
			if err != nil {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				sentBytes, err = w.Write(badJSONBytes)
				if err != nil {
					s.logger.Debug("Error writing response to client", zap.Error(err))
				}
				return
			}
		} else {
			payload = string(b)
		}
	}

	queryParams.Del("http_key")

	uid := ""
	if tokenAuth {
		uid = userID.String()
	}

	clientIP, clientPort := extractClientAddressFromRequest(s.logger, r)

	// Execute the function.
	result, fnErr, code := fn(r.Context(), queryParams, uid, username, vars, expiry, "", clientIP, clientPort, payload)
	if fnErr != nil {
		response, _ := json.Marshal(map[string]interface{}{"error": fnErr, "message": fnErr.Error(), "code": code})
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(grpcgw.HTTPStatusFromCode(code))
		sentBytes, err = w.Write(response)
		if err != nil {
			s.logger.Debug("Error writing response to client", zap.Error(err))
		}
		return
	}

	// Return the successful result.
	var response []byte
	if !unwrap {
		// GRPC Gateway equivalent behaviour.
		var err error
		response, err = json.Marshal(map[string]interface{}{"payload": result})
		if err != nil {
			// Failed to encode the wrapped response.
			s.logger.Error("Error marshaling wrapped response to client", zap.Error(err))
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			sentBytes, err = w.Write(internalServerErrorBytes)
			if err != nil {
				s.logger.Debug("Error writing response to client", zap.Error(err))
			}
			return
		}
	} else {
		// "Unwrapped" response.
		response = []byte(result)
	}
	if unwrap {
		if contentType := r.Header["Content-Type"]; len(contentType) > 0 {
			// Assume the request input content type is the same as the expected response.
			w.Header().Set("content-type", contentType[0])
		} else {
			// Don't know payload content-type.
			w.Header().Set("content-type", "text/plain")
		}
	} else {
		// Fall back to default response content type application/json.
		w.Header().Set("content-type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	sentBytes, err = w.Write(response)
	if err != nil {
		s.logger.Debug("Error writing response to client", zap.Error(err))
		return
	}
	success = true
}

func (s *ApiServer) RpcFunc(ctx context.Context, in *api.Rpc) (*api.Rpc, error) {
	if in.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "RPC ID must be set")
	}

	id := strings.ToLower(in.Id)

	fn := s.runtime.Rpc(id)
	if fn == nil {
		return nil, status.Error(codes.NotFound, "RPC function not found")
	}

	queryParams := make(map[string][]string, 0)
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Internal, "RPC function could not get incoming context")
	}
	for k, vs := range md {
		// Only process the keys representing custom query parameters.
		if strings.HasPrefix(k, "q_") {
			queryParams[k[2:]] = vs
		}
	}

	uid := ""
	username := ""
	var vars map[string]string
	expiry := int64(0)
	if u := ctx.Value(ctxUserIDKey{}); u != nil {
		uid = u.(uuid.UUID).String()
	}
	if u := ctx.Value(ctxUsernameKey{}); u != nil {
		username = u.(string)
	}
	if v := ctx.Value(ctxVarsKey{}); v != nil {
		vars = v.(map[string]string)
	}
	if e := ctx.Value(ctxExpiryKey{}); e != nil {
		expiry = e.(int64)
	}

	clientIP, clientPort := extractClientAddressFromContext(s.logger, ctx)

	result, fnErr, code := fn(ctx, queryParams, uid, username, vars, expiry, "", clientIP, clientPort, in.Payload)
	if fnErr != nil {
		return nil, status.Error(code, fnErr.Error())
	}

	return &api.Rpc{Payload: result}, nil
}
