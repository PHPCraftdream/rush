package agent

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func escrowTestCreds(key string) *CredentialSet {
	return &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: key}},
		Models:      map[Role]ModelChoice{RoleSmart: {Provider: "tenant", Model: "tenant-model"}},
	}
}

func TestBatchCredentialEscrowRegisterConsumeRelease(t *testing.T) {
	e := newBatchCredentialEscrow()
	key := batchCredentialKey{registryID: "reg", sessionID: "sess", messageID: "msg", callID: "call", tool: "agent"}
	creds := escrowTestCreds("tenant-secret")
	releaseRegistration, err := e.registerBatch([]batchCredentialRegistration{{key: key, root: "root", creds: creds}})
	require.NoError(t, err)
	creds.Credentials[0].APIKey = "changed"
	got, complete, err := e.consume(key, "root.1")
	require.NoError(t, err)
	require.Equal(t, "tenant-secret", got.Credentials[0].APIKey)
	_, _, err = e.consume(key, "root.1")
	require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
	releaseRegistration()
	require.NoError(t, e.register(key, "root", escrowTestCreds("tenant-secret")))
	_, complete2, err := e.consume(key, "root.1")
	require.NoError(t, err)
	e.releaseRoot("root")
	require.Equal(t, "tenant-secret", got.Credentials[0].APIKey)
	complete()
	complete2()
	_, _, err = e.consume(key, "root.1")
	require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
}

func TestBatchCredentialEscrowConcurrentIsolationAndSingleConsume(t *testing.T) {
	e := newBatchCredentialEscrow()
	keys := []batchCredentialKey{
		{registryID: "r1", sessionID: "s1", messageID: "m", callID: "c", tool: "agent"},
		{registryID: "r2", sessionID: "s2", messageID: "m", callID: "c", tool: "agent"},
	}
	for i, key := range keys {
		require.NoError(t, e.register(key, "root", escrowTestCreds([]string{"one", "two"}[i])))
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	type keyedResult struct {
		session string
		secret  string
	}
	results := make(chan keyedResult, 2)
	for _, key := range keys {
		wg.Add(1)
		go func(key batchCredentialKey) {
			defer wg.Done()
			<-start
			creds, complete, err := e.consume(key, "root.1")
			if err != nil {
				results <- keyedResult{session: key.sessionID, secret: "error"}
				return
			}
			results <- keyedResult{session: key.sessionID, secret: creds.Credentials[0].APIKey}
			complete()
		}(key)
	}
	close(start)
	wg.Wait()
	close(results)
	seen := map[string]string{}
	for result := range results {
		seen[result.session] = result.secret
	}
	require.Equal(t, map[string]string{"s1": "one", "s2": "two"}, seen)
}

func TestBatchCredentialEscrowReleaseRacesConsume(t *testing.T) {
	for _, releaseFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "release first", false: "consume first"}[releaseFirst], func(t *testing.T) {
			e := newBatchCredentialEscrow()
			key := batchCredentialKey{registryID: "r", sessionID: "s", messageID: "m", callID: "c", tool: "agent"}
			require.NoError(t, e.register(key, "root", escrowTestCreds("secret")))
			type result struct {
				creds *CredentialSet
				err   error
			}
			start, consumeGate, releaseGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseDone := make(chan struct{})
			consumed := make(chan result, 1)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				if releaseFirst {
					<-releaseGate
					e.releaseRoot("root")
					close(consumeGate)
					close(releaseDone)
					return
				}
				<-consumeGate
				<-releaseGate
				e.releaseRoot("root")
				close(releaseDone)
			}()
			go func() {
				defer wg.Done()
				<-start
				if releaseFirst {
					<-consumeGate
					creds, complete, err := e.consume(key, "root.1")
					consumed <- result{creds: creds, err: err}
					if complete != nil {
						complete()
					}
					return
				}
				creds, complete, err := e.consume(key, "root.1")
				consumed <- result{creds: creds, err: err}
				<-releaseDone
				if err == nil {
					complete()
				}
			}()
			close(start)
			if releaseFirst {
				close(releaseGate)
				result := <-consumed
				wg.Wait()
				require.ErrorIs(t, result.err, errBatchCredentialEscrowMissing)
				require.Nil(t, result.creds)
			} else {
				close(consumeGate)
				result := <-consumed
				close(releaseGate)
				wg.Wait()
				require.NoError(t, result.err)
				require.Equal(t, "secret", result.creds.Credentials[0].APIKey)
			}
			_, _, err := e.consume(key, "root.1")
			require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
		})
	}
}

func TestBatchCredentialEscrowMissingAndWrongRootFailClosed(t *testing.T) {
	e := newBatchCredentialEscrow()
	key := batchCredentialKey{registryID: "r", sessionID: "s", messageID: "m", callID: "c", tool: "agent"}
	require.NoError(t, e.register(key, "root", nil))
	_, _, err := e.consume(key, "other.1")
	require.ErrorIs(t, err, errBatchCredentialEscrowMissing)
	creds, complete, err := e.consume(key, "root.1")
	require.NoError(t, err)
	require.Nil(t, creds)
	complete()
}
