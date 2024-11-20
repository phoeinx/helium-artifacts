package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/ChristianMct/helium"
	"github.com/ChristianMct/helium/circuits"
	"github.com/ChristianMct/helium/objectstore"
	"github.com/ChristianMct/helium/protocols"
	"github.com/ChristianMct/helium/services/compute"
	"github.com/ChristianMct/helium/services/setup"
	"github.com/ChristianMct/helium/sessions"
	"github.com/tuneinsight/lattigo/v5/core/rlwe"
	"github.com/tuneinsight/lattigo/v5/he"
	"github.com/tuneinsight/lattigo/v5/mhe"
	"github.com/tuneinsight/lattigo/v5/schemes/bgv"
	"golang.org/x/exp/maps"
	"gonum.org/v1/gonum/mat"

	"github.com/ChristianMct/helium/node"
)

// cloudAddress is the local address that the cloud node listens on.
const cloudAddress = ":40000"
var nInputNodes int = 0

// defines the command-line flags
var (
	nodeId       = flag.String("node_id", "", "the id of the node")
	nParty       = flag.Int("n_party", -1, "the number of parties")
	cloudAddr    = flag.String("cloud_address", "", "the address of the helper node")
	argThreshold = flag.Int("threshold", -1, "the threshold")
	expRounds    = flag.Int("expRounds", 1, "number of circuit evaluatation rounds to perform")
)

func main() {

	flag.Parse()

	if *nParty < 2 {
		panic("n_party argument should be provided and > 2")
	}

	if len(*nodeId) == 0 {
		panic("node_id argument should be provided")
	}

	if len(*cloudAddr) == 0 {
		panic("cloud_address argument must be provided for session nodes")
	}

	nInputNodes = *nParty - 1

	// sets the threshold to the number of parties if not provided
	var threshold int
	switch {
	case *argThreshold == -1:
		threshold = *nParty
	case *argThreshold > 0 && *argThreshold <= *nParty:
		threshold = *argThreshold
	default:
		flag.Usage()
		panic("threshold argument must be between 1 and N")
	}

	nid := sessions.NodeID(*nodeId)

	// generates a test node list from the command-line arguments
	nids, nl, shamirPks, nodeMapping := genNodeLists(*nParty, *cloudAddr)

	// generates a config for the node running this program
	nc := genConfigForNode(nid, nids, threshold, shamirPks)

	// retreives the session parameters from the node config
	params, err := bgv.NewParametersFromLiteral(nc.SessionParameters[0].FHEParameters.(bgv.ParametersLiteral))
	if err != nil {
		panic(err)
	}
	fmt.Println("Max Slots", params.MaxSlots())

	// the matrix size
	m := params.MaxSlots() / 2

	// generates a test matrix
	a := genTestMatrix(m)

	// generates the Helium application (see helium/node/app.go).
	// The app declares a circuit "matmul4-dec" that computes the
	// encrypted matrix-vector product followed by a collective
	// decryption.
	app := getApp(params, m)

	// creates a context for the session
	ctx := sessions.NewBackgroundContext(nc.SessionParameters[0].ID)

	// runs Helium as a server or client
	var timeSetup, timeCompute time.Duration
	var stats map[string]interface{}
	start := time.Now()
	if nc.ID == "cloud" {

		// runs the Helium server. The method returns when the setup phase has completed.
		// It returns a channel to send circuit descriptors (evaluation requests) and a channel to
		// receive the evaluation outputs.
		hsv, cdescs, outs, err := helium.RunHeliumServer(ctx, nc, nl, app, compute.NoInput)

		if err != nil {
			log.Fatalf("error running helium server: %v", err)
		}
		timeSetup = time.Since(start)
		fmt.Println("Time Setup", timeSetup)

		// One the setup has completed, the collective public key is available
		// and the test matrix can be encrypted with it.
		if err := encryptTestMatrix(ctx, a, params, hsv, hsv); err != nil {
			log.Fatalf("error encrypting test matrix: %v", err)
		}

		fmt.Println("Encrypted test matrix")

		start = time.Now()
		// sends *expRounds evaluation requests to the server for circuit "matmul4-dec".
		go func() {
			var nSig int
			for i := 0; i < *expRounds; i++ {
				cdescs <- circuits.Descriptor{
					Signature:   circuits.Signature{Name: "vecadd4-dec"},
					CircuitID:   sessions.CircuitID(fmt.Sprintf("vecadd-%d", nSig)),
					// Signature:   circuits.Signature{Name: "matmul4-dec"},
					// CircuitID:   sessions.CircuitID(fmt.Sprintf("matmul-%d", nSig)),
					NodeMapping: nodeMapping,
					Evaluator:   "cloud",
				}
				nSig++
				fmt.Println("Sent", nSig)
			}
			close(cdescs)
		}()

		// the cloud is not supposed to receive any output
		out, hasOut := <-outs
		// Log output
		fmt.Printf("Has output", hasOut)
		if hasOut {
			encoder := bgv.NewEncoder(params)
			pt := &rlwe.Plaintext{Element: out.Ciphertext.Element, Value: out.Ciphertext.Value[0]}
			pt.IsBatched = true
			res := make([]uint64, params.MaxSlots())
			err := encoder.Decode(pt, res)
			if err != nil {
				log.Fatalf("%s | [main] error decoding output: %v\n", nc.ID, err)
			}
			res = res[:m]
			if err != nil {
				log.Fatalf("%s | [main] error decoding output: %v\n", nc.ID, err)
			}
			fmt.Printf("%v\n", res)
		}
		

		hsv.GracefulStop() // waits for the last client to disconnect
		timeCompute = time.Since(start)
		stats = map[string]interface{}{
			"Time": map[string]interface{}{
				"Setup":   timeSetup,
				"Compute": timeCompute,
			},
			"Net": hsv.GetStats(),
		}
	} else {

		// creates an input provider function for the node (see getInputProvider).
		encoder := bgv.NewEncoder(params)
		var ip compute.InputProvider = getInputProvider(params, encoder, m)

		// runs the Helium client. The method returns a channel to receive the evaluation outputs
		// for which the node is the receiver.
		hc, outs, err := helium.RunHeliumClient(ctx, nc, nl, app, ip)
		if err != nil {
			log.Fatalf("error running helium client: %v", err)
		}

		// checks the results
		for out := range outs {
			if err = checkResultCorrect(params, *encoder, out, a); err != nil {
				log.Printf("error checking result: %v", err)
			} else {
				log.Printf("got correct result for %s", out.OperandLabel)
			}
		}

		if err := hc.Close(); err != nil {
			log.Fatalf("error closing helium client: %v", err)
		}
		stats = map[string]interface{}{
			"net": hc.GetStats(),
		}
	}

	//outputs the stats as JSON on stdout
	statsJson, err := json.Marshal(stats)
	if err != nil {
		log.Fatalf("error marshalling stats: %v", err)
	}
	fmt.Println("STATS", string(statsJson))
}

// genNodeLists generates a test list of node informations from the experiments parameters.
// In a real scenarios, the node informations would be provided by the user application.
func genNodeLists(nParty int, cloudAddr string) (nids []sessions.NodeID, nl node.List, shamirPks map[sessions.NodeID]mhe.ShamirPublicPoint, nodeMapping map[string]sessions.NodeID) {
	nids = make([]sessions.NodeID, nParty)
	nl = make(node.List, nParty)
	shamirPks = make(map[sessions.NodeID]mhe.ShamirPublicPoint, nParty)
	nodeMapping = make(map[string]sessions.NodeID, nParty+2)
	nodeMapping["cloud"] = "cloud"
	for i := range nids {
		nids[i] = sessions.NodeID(fmt.Sprintf("node-%d", i))
		nl[i].NodeID = nids[i]
		shamirPks[nids[i]] = mhe.ShamirPublicPoint(i + 1)
		nodeMapping[string(nids[i])] = nids[i]
	}
	nl = append(nl, struct {
		sessions.NodeID
		node.Address
	}{NodeID: "cloud", Address: node.Address(cloudAddr)})
	// Print out the nodeMapping map
	for k, v := range nodeMapping {
		fmt.Printf("Key: %s, Value: %s\n", k, v)
	}

	return
}

// genConfigForNode generates a node.Config for the node with the provided node ID. It also simulates the loading of the secret-key for the node.
// In a real scenario, the secret-key would be loaded from a secure storage.
func genConfigForNode(nid sessions.NodeID, nids []sessions.NodeID, threshold int, shamirPks map[sessions.NodeID]mhe.ShamirPublicPoint) (nc node.Config) {
	sessParams := sessions.Parameters{
		ID:            "test-session",
		Nodes:         nids,
		//FHEParameters: bgv.ParametersLiteral{PlaintextModulus: 65537, LogN: 14, LogQ: []int{56, 55, 55, 54, 54, 54}, LogP: []int{55, 55}},
		//TODO: Test other parameters when not creating the encrypted matrix.
		FHEParameters: bgv.ParametersLiteral{PlaintextModulus: 79873, LogN: 12, LogQ: []int{45, 45}, LogP: []int{19}},
		Threshold:     threshold,
		PublicSeed:    []byte{'c', 'r', 's'},
		ShamirPks:     shamirPks,
	}

	nc = node.Config{
		ID:                nid,
		HelperID:          "cloud",
		SessionParameters: []sessions.Parameters{sessParams},
		ObjectStoreConfig: objectstore.Config{BackendName: "mem"},
		TLSConfig:         node.TLSConfig{InsecureChannels: true},
		SetupConfig: setup.ServiceConfig{
			Protocols: protocols.ExecutorConfig{MaxProtoPerNode: 3, MaxParticipation: 3, MaxAggregation: 1},
		},
		ComputeConfig: compute.ServiceConfig{
			MaxCircuitEvaluation: 10,
			Protocols:            protocols.ExecutorConfig{MaxProtoPerNode: 3, MaxParticipation: 3, MaxAggregation: 1},
		},
	}

	if nid == "cloud" {
		nc.Address = cloudAddress
		nc.SetupConfig.Protocols.MaxAggregation = 32
		nc.ComputeConfig.Protocols.MaxAggregation = 32
	} else {
		var err error
		nc.SessionParameters[0].Secrets, err = loadSecrets(sessParams, nid)
		if err != nil {
			log.Fatalf("could not load node's secrets: %s", err)
		}
	}
	return
}

// getApp generates the Helium application for the test.
// The application specifies the setup phase and declares the circuits that can be executed by the nodes.
func getApp(params bgv.Parameters, m int) node.App {
	diagGalEl := make(map[int]uint64)
	for k := 0; k < m; k++ {
		diagGalEl[k] = params.GaloisElement(k)
	}
	return node.App{
		SetupDescription: &setup.Description{
			Cpk: true,
			Rlk: true,
			Gks: maps.Values(diagGalEl),
		},
		Circuits: map[circuits.Name]circuits.Circuit{
			//"matmul4-dec": matmul4dec,
			"vecadd4-dec": vecadd4dec,
		},
	}
}

// getInputProvider generates an input provider function for the node. The input provider function
// is registered to with the Helium node and is called by Helium to provide the input for the circuit evaluation.
func getInputProvider(params bgv.Parameters, encoder *bgv.Encoder, m int) compute.InputProvider {
	return func(ctx context.Context, ci sessions.CircuitID, ol circuits.OperandLabel, s sessions.Session) (any, error) {

		encoder := encoder.ShallowCopy()

		// Creates a vector of size m with the first element set to 1, the rest to 0.
		var pt *rlwe.Plaintext
		b := mat.NewVecDense(m, nil)
		b.SetVec(0, 1)
		data := make([]uint64, len(b.RawVector().Data))
		for i, _ := range b.RawVector().Data {
			data[i] = uint64(i)
		}

		pt = bgv.NewPlaintext(params, params.MaxLevelQ())
		err := encoder.Encode(data, pt)

		if err != nil {
			return nil, err
		}

		return pt, nil

	}
}

// checkResultCorrect checks if the result of the circuit evaluation is correct by computing the matrix-vector product.
func checkResultCorrect(params bgv.Parameters, encoder bgv.Encoder, out circuits.Output, a *mat.Dense) error {
	_, m := a.Dims()

	b := mat.NewVecDense(m, nil)
	b.SetVec(0, 1)
	r := mat.NewVecDense(m, nil)

	r.MulVec(a, b)
	dataWant := make([]uint64, len(r.RawVector().Data))
	for i, v := range r.RawVector().Data {
		dataWant[i] = uint64(v)
	}

	pt := &rlwe.Plaintext{Element: out.Ciphertext.Element, Value: out.Ciphertext.Value[0]}
	pt.IsBatched = true
	res := make([]uint64, params.MaxSlots())
	if err := encoder.Decode(pt, res); err != nil {
		return fmt.Errorf("error decoding result: %v", err)
	}
	res = res[:m]

	for i, v := range res {
		if v != dataWant[i] {
			return fmt.Errorf("incorrect result for %s: \n has %v, want %v\n", out.OperandLabel, res, dataWant)
		}
	}
	return nil
}

// genTestMatrix generates a test secret matrix of size mxm for the experiment.
func genTestMatrix(m int) *mat.Dense {
	a := mat.NewDense(m, m, nil)
	a.Apply(func(i, j int, v float64) float64 {
		return float64(i) + float64(2*j)
	}, a)
	return a
}

// encryptTestMatrix encrypts the test matrix with the collective public key of the session, and
// stores the encrypted matrix in the operand provider.
func encryptTestMatrix(ctx context.Context, a *mat.Dense, params bgv.Parameters, pkb circuits.PublicKeyProvider, opp compute.OperandProvider) error {

	cpk, err := pkb.GetCollectivePublicKey(ctx)
	if err != nil {
		return err
	}
	encryptor := bgv.NewEncryptor(params, cpk)
	encoder := bgv.NewEncoder(params)

	pta := make(map[int]*rlwe.Plaintext)
	cta := make(map[int]*rlwe.Ciphertext)

	_, m := a.Dims()
	diag := make(map[int][]uint64, m)
	for k := 0; k < m; k++ {
		diag[k] = make([]uint64, m)
		for i := 0; i < m; i++ {
			j := (i + k) % m
			diag[k][i] = uint64(a.At(i, j))
		}
	}

	log.Printf("generating encrypted matrix...")
	for di, d := range diag {
		pta[di] = bgv.NewPlaintext(params, params.MaxLevelQ())
		if err = encoder.Encode(d, pta[di]); err != nil {
			return err
		}
		if cta[di], err = encryptor.EncryptNew(pta[di]); err != nil {
			return err
		}
		op := &circuits.Operand{Ciphertext: cta[di], OperandLabel: circuits.OperandLabel(fmt.Sprintf("//cloud/mat-diag-%d", di))}
		if err := opp.PutOperand(op.OperandLabel, op); err != nil {
			return err
		}
	}
	log.Printf("done")
	return nil
}

// //matmul4dec is a circuit that computes the encrypted matrix-vector product followed by a collective decryption.
// func matmul4dec(e circuits.Runtime) error {
// 	params := e.Parameters().(bgv.Parameters)

// 	m := params.MaxSlots() / 2

// 	vecOp := e.Input(circuits.OperandLabel("//node-0/vec"))

// 	matOps := make(map[int]*circuits.Operand)
// 	diagGalEl := make(map[int]uint64)
// 	for k := 0; k < m; k++ {
// 		matOps[k] = e.Load(circuits.OperandLabel(fmt.Sprintf("//cloud/mat-diag-%d", k)))
// 		diagGalEl[k] = params.GaloisElement(k)
// 	}

// 	opRes := e.NewOperand("//cloud/res-0")
// 	if err := e.EvalLocal(true, maps.Values(diagGalEl), func(e he.Evaluator) error {
// 		opRes.Ciphertext = bgv.NewCiphertext(params, 1, params.MaxLevel())

// 		eval, isBgv := e.(*bgv.Evaluator)
// 		if !isBgv {
// 			return fmt.Errorf("evaluator is not a *bgv.Evaluator, is %T", e)
// 		}
// 		eval.DecomposeNTT(params.MaxLevelQ(), params.MaxLevelP(), params.PCount(), vecOp.Get().Value[1], true, eval.BuffDecompQP)
// 		vecRotated := bgv.NewCiphertext(params, 1, params.MaxLevelQ())
// 		ctprod := bgv.NewCiphertext(params, 2, params.MaxLevel())
// 		for di, d := range matOps {
// 			if err := eval.AutomorphismHoisted(vecOp.LevelQ(), vecOp.Ciphertext, eval.BuffDecompQP, diagGalEl[di], vecRotated); err != nil {
// 				return err
// 			}
// 			if err := e.MulThenAdd(vecRotated, d.Ciphertext, ctprod); err != nil {
// 				return err
// 			}
// 		}
// 		return e.Relinearize(ctprod, opRes.Ciphertext)
// 	}); err != nil {
// 		return err
// 	}

// 	return e.DEC(*opRes, "node-0", map[string]string{
// 		"smudging": "40.0",
// 	})
// }


func vecadd4dec(rt circuits.Runtime) error {
	//TODO: Possible to make this parameterized without a global variable? / Obtain this information from Runtime?
	nodeCount := nInputNodes
	fmt.Println("Running vecadd4dec")
	inputs := make(map[int]*circuits.FutureOperand)
	
	for i := 0; i < nodeCount; i++ {
		inputs[i] = rt.Input(circuits.OperandLabel(fmt.Sprintf("//node-%d/vec", i)))
	}

	fmt.Println("Number of inputs", len(inputs))
	// computes the addition of all inputs
	opRes := rt.NewOperand("//cloud/res-0")
	if err := rt.EvalLocal(true, nil, func(eval he.Evaluator) error {
		var sum *rlwe.Ciphertext
		var err error
		sum = inputs[0].Get().Ciphertext
		for i := 1; i < nodeCount; i++ {
			fmt.Println("Adding", i)
			if sum, err = eval.AddNew(sum, inputs[i].Get().Ciphertext); err != nil {
				return err
			}
		}
		opRes.Ciphertext = sum
		return err

	}); err != nil {
		return err
	}

	// decrypts the result with result receiver id "rec". The node id can be a place-holder and the actual id is provided
	// when querying for a circuit's execution.
	return rt.DEC(*opRes, "cloud", map[string]string{
		"smudging": "40.0", // use 40 bits of smudging.
	})
}

// simulates loading the secrets. In a real application, the secrets would be loaded from a secure storage.
func loadSecrets(sp sessions.Parameters, nid sessions.NodeID) (secrets *sessions.Secrets, err error) {

	ss, err := sessions.GenTestSecretKeys(sp)
	if err != nil {
		return nil, err
	}

	secrets, ok := ss[nid]
	if !ok {
		return nil, fmt.Errorf("node %s not in session", nid)
	}

	return
}
