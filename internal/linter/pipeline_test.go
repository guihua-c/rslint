package linter

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/typescript-go/shim/ast"
	"github.com/microsoft/typescript-go/shim/bundled"
	"github.com/microsoft/typescript-go/shim/core"
	"github.com/microsoft/typescript-go/shim/tspath"
	"github.com/microsoft/typescript-go/shim/vfs/osvfs"

	"github.com/web-infra-dev/rslint/internal/program"
	"github.com/web-infra-dev/rslint/internal/rule"
	"github.com/web-infra-dev/rslint/internal/utils"
)

func pipelineTestProgram(t *testing.T, root string, fileName string, text string) *program.Program {
	t.Helper()
	fs := utils.NewOverlayVFS(bundled.WrapFS(osvfs.FS()), map[string]string{fileName: text})
	result, err := program.NewFromRoots(program.RootOptions{
		RootFileNames:   []string{fileName},
		Host:            utils.CreateCompilerHost(root, fs),
		CompilerOptions: &core.CompilerOptions{},
		SingleThreaded:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func pipelineTestGeneration(
	t *testing.T,
	root string,
	fileName string,
	text string,
	configuredRules []rule.ConfiguredRule,
	pluginConfig *EslintPluginFileConfig,
) Generation {
	t.Helper()
	result := Generation{
		Native: NativeGeneration{
			Programs:         []*program.Program{pipelineTestProgram(t, root, fileName, text)},
			TargetsByProgram: [][]string{{fileName}},
			SingleThreaded:   true,
			Cwd:              root,
			RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
				return configuredRules
			},
		},
		Target: TargetProjection{
			Path: func(string) string { return fileName },
			ReadText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			},
		},
	}
	if pluginConfig != nil {
		result.Plugin = &PluginGeneration{
			ConfigForFile: func(string) EslintPluginFileConfig { return *pluginConfig },
		}
	}
	return result
}

func pipelineTestProvider(generation Generation, release ReleaseFunc) GenerationProvider {
	return GenerationProviderFunc(func(context.Context, SourceSnapshot) (Generation, ReleaseFunc, error) {
		return generation, release, nil
	})
}

func runPipelineWithParallelRuleResolver(
	t *testing.T,
	resolver func(*ast.SourceFile) []rule.ConfiguredRule,
) (recovered any, releases int32) {
	t.Helper()
	root := tspath.NormalizePath(t.TempDir())
	firstPath := tspath.ResolvePath(root, "first.ts")
	secondPath := tspath.ResolvePath(root, "second.ts")
	generation := Generation{Native: NativeGeneration{
		Programs: []*program.Program{
			pipelineTestProgram(t, root, firstPath, "const first = 1;"),
			pipelineTestProgram(t, root, secondPath, "const second = 2;"),
		},
		TargetsByProgram: [][]string{{firstPath}, {secondPath}},
		RulesForFile:     resolver,
	}}
	var releaseCount atomic.Int32
	func() {
		defer func() { recovered = recover() }()
		_, _ = RunPipeline(context.Background(), NewLintRequest(
			pipelineTestProvider(generation, func() { releaseCount.Add(1) }),
			ObservationPolicy{},
			nil,
		))
	}()
	return recovered, releaseCount.Load()
}

type pipelineProgressiveDiagnostics struct {
	baseline  []rule.RuleDiagnostic
	parentCtx context.Context
	run       DeferredPluginRun
	onPublish func()
	onSubmit  func()
}

func (p *pipelineProgressiveDiagnostics) PublishBaseline(
	_ context.Context,
	diagnostics []rule.RuleDiagnostic,
) {
	if p.onPublish != nil {
		p.onPublish()
	}
	p.baseline = append([]rule.RuleDiagnostic(nil), diagnostics...)
}

func (p *pipelineProgressiveDiagnostics) Submit(parentCtx context.Context, run DeferredPluginRun) {
	if p.onSubmit != nil {
		p.onSubmit()
	}
	p.parentCtx = parentCtx
	p.run = run
}

func TestPipelineReleasesGenerationOnPreparationFailureAndPanic(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		var releases atomic.Int32
		_, err := RunPipeline(context.Background(), NewLintRequest(
			pipelineTestProvider(Generation{Native: NativeGeneration{
				Programs: []*program.Program{nil},
				RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
					return nil
				},
			}}, func() {
				releases.Add(1)
			}),
			ObservationPolicy{},
			nil,
		))
		if err == nil || releases.Load() != 1 {
			t.Fatalf("error/releases = %v/%d, want error/1", err, releases.Load())
		}
	})

	t.Run("panic", func(t *testing.T) {
		root := tspath.NormalizePath(t.TempDir())
		fileName := tspath.ResolvePath(root, "source.ts")
		generation := pipelineTestGeneration(t, root, fileName, "const value = 1;", nil, nil)
		generation.Native.RulesForFile = func(*ast.SourceFile) []rule.ConfiguredRule {
			panic("resolver failed")
		}
		var releases atomic.Int32
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_, _ = RunPipeline(context.Background(), NewLintRequest(
				pipelineTestProvider(generation, func() { releases.Add(1) }),
				ObservationPolicy{},
				nil,
			))
		}()
		if recovered == nil || releases.Load() != 1 {
			t.Fatalf("panic/releases = %v/%d, want panic/1", recovered, releases.Load())
		}
	})

	t.Run("parallel panics", func(t *testing.T) {
		previousProcs := runtime.GOMAXPROCS(2)
		defer runtime.GOMAXPROCS(previousProcs)

		var arrivals atomic.Int32
		releaseResolvers := make(chan struct{})
		recovered, releases := runPipelineWithParallelRuleResolver(t, func(source *ast.SourceFile) []rule.ConfiguredRule {
			if arrivals.Add(1) == 2 {
				close(releaseResolvers)
			}
			select {
			case <-releaseResolvers:
			case <-time.After(5 * time.Second):
				panic("timed out waiting for both parallel resolvers")
			}
			panic(source.FileName())
		})
		firstPanic, firstOK := recovered.(string)
		if !firstOK ||
			(!strings.HasSuffix(firstPanic, "/first.ts") && !strings.HasSuffix(firstPanic, "/second.ts")) ||
			releases != 1 {
			t.Fatalf("parallel panic/releases = %v/%d, want either resolver panic/1", recovered, releases)
		}
	})

	t.Run("parallel abnormal worker exit", func(t *testing.T) {
		previousProcs := runtime.GOMAXPROCS(2)
		defer runtime.GOMAXPROCS(previousProcs)

		var calls atomic.Int32
		recovered, releases := runPipelineWithParallelRuleResolver(t, func(*ast.SourceFile) []rule.ConfiguredRule {
			if calls.Add(1) == 1 {
				runtime.Goexit()
			}
			return nil
		})
		recoveredErr, ok := recovered.(error)
		if !ok || !errors.Is(recoveredErr, errPlanWorkerAborted) || releases != 1 {
			t.Fatalf("abnormal worker exit/releases = %v/%d, want %v/1", recovered, releases, errPlanWorkerAborted)
		}
	})
}

func TestPipelineRejectsInvalidGenerationPortsBeforeExecution(t *testing.T) {
	t.Run("nil provider function", func(t *testing.T) {
		_, err := RunPipeline(context.Background(), NewLintRequest(
			GenerationProviderFunc(nil),
			ObservationPolicy{},
			nil,
		))
		if err == nil || !strings.Contains(err.Error(), "provider function must not be nil") {
			t.Fatalf("nil provider error = %v", err)
		}
	})

	t.Run("empty target projection", func(t *testing.T) {
		root := tspath.NormalizePath(t.TempDir())
		fileName := tspath.ResolvePath(root, "source.ts")
		ruleRan := false
		generation := pipelineTestGeneration(t, root, fileName, "const value = 1;", []rule.ConfiguredRule{{
			Name: "native/must-not-run",
			Run: func(rule.RuleContext) rule.RuleListeners {
				ruleRan = true
				return nil
			},
		}}, nil)
		generation.Target.Path = func(string) string { return "" }
		_, err := RunPipeline(context.Background(), NewLintRequest(
			pipelineTestProvider(generation, nil),
			ObservationPolicy{},
			nil,
		))
		if err == nil || !strings.Contains(err.Error(), "projected target path must not be empty") || ruleRan {
			t.Fatalf("projection error/rule ran = %v/%v", err, ruleRan)
		}
	})
}

func TestPipelineAcceptsGenerationWithoutLintPlan(t *testing.T) {
	result, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(Generation{Native: NativeGeneration{SingleThreaded: true}}, nil),
		ObservationPolicy{},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if result.Observation.Native.Lint == nil ||
		result.Observation.Native.Lint.LintedFileCount != 0 ||
		len(result.Observation.Native.Diagnostics) != 0 ||
		len(result.Observation.Native.Files) != 0 ||
		result.Observation.Native.HasTargetSyntaxErrors {
		t.Fatalf("empty generation observation = %+v", result.Observation.Native)
	}
}

func TestPipelineCollectsLintedFilesOnlyWhenDemanded(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(t, root, fileName, "const value = 1;", nil, nil)

	withoutFiles, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if withoutFiles.Observation.Native.Files != nil {
		t.Fatalf("unrequested linted files = %+v", withoutFiles.Observation.Native.Files)
	}

	withFiles, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Demand: ArtifactDemand{LintedFiles: true}},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	files := withFiles.Observation.Native.Files
	if len(files) != 1 || files[0].Path != fileName || files[0].SourceFile == nil {
		t.Fatalf("requested linted files = %+v", files)
	}
}

func TestPipelineConcurrentPluginStartsBeforeNativeAndPlansProjectedFix(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	projectedPath := "target:" + fileName
	pluginStarted := make(chan struct{})
	allowPluginFinish := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nativeRan := false
	configuredRules := []rule.ConfiguredRule{
		{
			Name:     "native/order",
			Severity: rule.SeverityWarning,
			Run: func(rule.RuleContext) rule.RuleListeners {
				select {
				case <-pluginStarted:
				case <-ctx.Done():
					t.Fatal("native work started before plugin dispatch")
				}
				nativeRan = true
				close(allowPluginFinish)
				return nil
			},
		},
		{Name: "plugin/replace", Severity: rule.SeverityError, IsEslintPluginRule: true},
	}
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		configuredRules,
		&EslintPluginFileConfig{ConfigKey: "config"},
	)
	generation.Target.Path = func(string) string { return projectedPath }
	result, err := RunPipeline(ctx, NewAutofixRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{
			Demand: ArtifactDemand{Native: rule.EditDemandAutofix, Plugin: rule.EditDemandAutofix},
			Plugin: PluginConcurrentJoined,
		},
		AutofixPolicy{MaxRounds: 1},
		func(_ context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			if request.Files[0].Path != fileName {
				t.Fatalf("plugin wire path = %q, want Program source path %q", request.Files[0].Path, fileName)
			}
			close(pluginStarted)
			<-allowPluginFinish
			return &EslintPluginLintResult{Results: []EslintPluginFileResult{{
				FilePath: request.Files[0].Path,
				Diagnostics: []EslintPluginDiagnostic{{
					RuleName: "plugin/replace",
					Message:  "replace",
					StartPos: 0,
					EndPos:   1,
					Fixes:    []EslintPluginFix{{Range: [2]int{0, 1}, Text: "b"}},
				}},
			}}}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, complete := result.Observation.CompleteDiagnostics()
	applied, planned := result.AppliedFixes()
	changes := applied.FinalChanges
	if !nativeRan || !complete || len(diagnostics) != 1 || diagnostics[0].FilePath != projectedPath {
		t.Fatalf("observation = native:%v complete:%v diagnostics:%+v", nativeRan, complete, diagnostics)
	}
	if !planned || len(changes) != 1 || changes[0].Path != projectedPath || changes[0].Before != "a" || changes[0].After != "b" {
		t.Fatalf("planned changes = %+v, planned=%v", changes, planned)
	}
}

func TestPipelineJoinedModesPropagateCallerCancellation(t *testing.T) {
	for _, mode := range []PluginExecution{PluginConcurrentJoined, PluginAfterNativeJoined} {
		t.Run(fmt.Sprintf("mode-%d", mode), func(t *testing.T) {
			root := tspath.NormalizePath(t.TempDir())
			fileName := tspath.ResolvePath(root, "source.ts")
			nativeFinished := make(chan struct{})
			generation := pipelineTestGeneration(
				t,
				root,
				fileName,
				"a",
				[]rule.ConfiguredRule{
					{
						Name: "native/marker",
						Run: func(rule.RuleContext) rule.RuleListeners {
							close(nativeFinished)
							return nil
						},
					},
					{Name: "plugin/check", IsEslintPluginRule: true},
				},
				&EslintPluginFileConfig{},
			)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := RunPipeline(ctx, NewLintRequest(
				pipelineTestProvider(generation, nil),
				ObservationPolicy{
					Demand:        ArtifactDemand{Plugin: rule.EditDemandAll},
					Plugin:        mode,
					PluginFailure: PluginDiscardOnFailure,
				},
				func(dispatchCtx context.Context, _ EslintPluginLintRequest) (*EslintPluginLintResult, error) {
					<-nativeFinished
					cancel()
					return nil, dispatchCtx.Err()
				},
			))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pipeline error = %v, want context.Canceled", err)
			}
			records := result.PluginOutcomes()
			if len(records) != 1 || records[0].Observation != 0 || !errors.Is(records[0].DispatchError, context.Canceled) {
				t.Fatalf("plugin records = %+v, want canceled observation 0", records)
			}
			if result.Observation.Native.Lint != nil {
				t.Fatal("canceled observation was published as authoritative")
			}
		})
	}
}

func TestPipelineDoesNotPromoteIndependentPluginBudgetFailure(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		&EslintPluginFileConfig{},
	)
	result, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{
			Plugin:        PluginAfterNativeJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			return nil, context.DeadlineExceeded
		},
	))
	if err != nil {
		t.Fatalf("pipeline promoted an independent plugin budget failure: %v", err)
	}
	outcome, ok := result.Observation.JoinedPluginOutcome()
	if !ok || !errors.Is(outcome.DispatchError, context.DeadlineExceeded) {
		t.Fatalf("joined plugin outcome = %+v, want deadline failure", outcome)
	}
}

func TestPipelineRejectsMissingJoinedPluginDispatcher(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		&EslintPluginFileConfig{},
	)
	_, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Plugin: PluginConcurrentJoined},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "requires a dispatcher") {
		t.Fatalf("missing dispatcher error = %v", err)
	}
}

func TestPipelineFreezesCompletePluginInputAfterMemoryChanges(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	paths := []string{
		tspath.ResolvePath(root, "changed.ts"),
		tspath.ResolvePath(root, "unchanged.ts"),
	}
	initial := map[string]string{paths[0]: "a", paths[1]: "stable"}
	acquisitions := 0
	provider := GenerationProviderFunc(func(_ context.Context, snapshot SourceSnapshot) (Generation, ReleaseFunc, error) {
		acquisitions++
		texts := map[string]string{paths[0]: initial[paths[0]], paths[1]: initial[paths[1]]}
		for _, file := range snapshot.Files() {
			texts[file.Path] = file.Text
		}
		programs := []*program.Program{
			pipelineTestProgram(t, root, paths[0], texts[paths[0]]),
			pipelineTestProgram(t, root, paths[1], texts[paths[1]]),
		}
		return Generation{
			Native: NativeGeneration{
				Programs:         programs,
				TargetsByProgram: [][]string{{paths[0]}, {paths[1]}},
				SingleThreaded:   true,
				Cwd:              root,
				RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
					rules := []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
					if source.FileName() == paths[0] && source.Text() == "a" {
						rules = append(rules, rule.ConfiguredRule{
							Name: "native/fix",
							Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
								textRange := core.NewTextRange(0, 1)
								ruleCtx.ReportRangeWithFixes(
									textRange,
									rule.RuleMessage{Description: "fix"},
									rule.RuleFix{Range: textRange, Text: "b"},
								)
								return nil
							},
						})
					}
					return rules
				},
			},
			Target: TargetProjection{ReadText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			}},
			Plugin: &PluginGeneration{HostReadsInitialText: true},
		}, nil, nil
	})

	dispatches := 0
	var dispatchValidationErr error
	expectedSecond := map[string]string{paths[0]: "b", paths[1]: "stable"}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{
			Demand: ArtifactDemand{Native: rule.EditDemandAutofix},
			Plugin: PluginConcurrentJoined,
		},
		AutofixPolicy{MaxRounds: 1, VerifyAfterLastRound: true},
		func(_ context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			dispatches++
			if len(request.Files) != 2 {
				dispatchValidationErr = fmt.Errorf("plugin files = %+v", request.Files)
			}
			for _, file := range request.Files {
				if dispatches == 1 && file.Text != nil {
					dispatchValidationErr = fmt.Errorf("initial disk-backed input %q was inlined", file.Path)
				}
				if dispatches == 2 {
					if file.Text == nil || *file.Text != expectedSecond[file.Path] {
						dispatchValidationErr = fmt.Errorf("memory-generation input = %+v", file)
					}
				}
			}
			results := make([]EslintPluginFileResult, len(request.Files))
			for index, file := range request.Files {
				results[index].FilePath = file.Path
			}
			return &EslintPluginLintResult{Results: results}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || acquisitions != 2 || dispatches != 2 || len(applied.FinalChanges) != 1 || dispatchValidationErr != nil {
		t.Fatalf("result/acquisitions/dispatches/validation = %+v/%d/%d/%v", applied, acquisitions, dispatches, dispatchValidationErr)
	}
}

func TestPipelineRejectsDuplicatePluginWirePath(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	firstPath := tspath.ResolvePath(root, "first.ts")
	secondPath := tspath.ResolvePath(root, "second.ts")
	generation := Generation{
		Native: NativeGeneration{
			Programs: []*program.Program{
				pipelineTestProgram(t, root, firstPath, "a"),
				pipelineTestProgram(t, root, secondPath, "b"),
			},
			TargetsByProgram: [][]string{{firstPath}, {secondPath}},
			SingleThreaded:   true,
			Cwd:              root,
			RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
				return []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
			},
		},
		Plugin: &PluginGeneration{
			WirePath: func(string) string { return "shared.ts" },
		},
	}
	_, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Plugin: PluginConcurrentJoined},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			return &EslintPluginLintResult{}, nil
		},
	))
	if err == nil || !strings.Contains(err.Error(), "duplicate plugin wire path") {
		t.Fatalf("duplicate wire path error = %v", err)
	}
}

func TestPipelineRejectsDuplicateProjectedTargetBeforeExecution(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	firstPath := tspath.ResolvePath(root, "first.ts")
	secondPath := tspath.ResolvePath(root, "second.ts")
	ruleRan := false
	generation := Generation{
		Native: NativeGeneration{
			Programs: []*program.Program{
				pipelineTestProgram(t, root, firstPath, "a"),
				pipelineTestProgram(t, root, secondPath, "b"),
			},
			TargetsByProgram: [][]string{{firstPath}, {secondPath}},
			SingleThreaded:   true,
			Cwd:              root,
			RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
				return []rule.ConfiguredRule{{
					Name: "native/fix",
					Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
						ruleRan = true
						textRange := core.NewTextRange(0, len(source.Text()))
						ruleCtx.ReportRangeWithFixes(
							textRange,
							rule.RuleMessage{Description: "fix"},
							rule.RuleFix{Range: textRange, Text: "fixed"},
						)
						return nil
					},
				}}
			},
		},
		Target: TargetProjection{
			Path: func(string) string { return "shared.ts" },
			ReadText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			},
		},
	}
	_, err := RunPipeline(context.Background(), NewAutofixRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "duplicate projected target") || ruleRan {
		t.Fatalf("duplicate projected target error/rule ran = %v/%v", err, ruleRan)
	}
}

func TestFixSourcesRejectDistinctSourcesForSamePath(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	firstPath := tspath.ResolvePath(root, "first.ts")
	secondPath := tspath.ResolvePath(root, "second.ts")
	first := pipelineTestProgram(t, root, firstPath, "a").SourceFiles()[0]
	second := pipelineTestProgram(t, root, secondPath, "b").SourceFiles()[0]
	firstFixes := []rule.RuleFix{{Range: core.NewTextRange(0, 1), Text: "x"}}
	secondFixes := []rule.RuleFix{{Range: core.NewTextRange(0, 1), Text: "y"}}
	_, err := fixSourcesFromDiagnostics([]rule.RuleDiagnostic{
		{FilePath: "shared.ts", SourceFile: first, FixesPtr: &firstFixes},
		{FilePath: "shared.ts", SourceFile: second, FixesPtr: &secondFixes},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate fix target") {
		t.Fatalf("duplicate fix source error = %v", err)
	}
}

func TestProgressivePipelineChecksCancellationAfterRelease(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(t, root, fileName, "a", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	presentation := &pipelineProgressiveDiagnostics{}
	result, err := RunPipeline(ctx, NewProgressiveLintRequest(
		pipelineTestProvider(generation, ReleaseFunc(cancel)),
		ArtifactDemand{},
		presentation,
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipeline error = %v, want cancellation raised by release", err)
	}
	if result.Observation.Native.Lint != nil {
		t.Fatal("post-release canceled observation was published")
	}
	if presentation.baseline != nil || presentation.run != nil {
		t.Fatal("canceled progressive result reached presentation ports")
	}
}

func TestProgressivePipelineOwnsReleasePresentationGateAndSubmissionOrder(t *testing.T) {
	t.Run("eligible enrichment", func(t *testing.T) {
		root := tspath.NormalizePath(t.TempDir())
		fileName := tspath.ResolvePath(root, "source.ts")
		generation := pipelineTestGeneration(
			t,
			root,
			fileName,
			"const value = 1;",
			[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
			&EslintPluginFileConfig{},
		)
		released := false
		presented := false
		presentation := &pipelineProgressiveDiagnostics{
			onPublish: func() {
				if !released {
					t.Fatal("baseline was published before generation release")
				}
				presented = true
			},
			onSubmit: func() {
				if !presented {
					t.Fatal("enrichment was submitted before baseline publication")
				}
			},
		}
		result, err := RunPipeline(context.Background(), NewProgressiveLintRequest(
			pipelineTestProvider(generation, func() { released = true }),
			ArtifactDemand{Plugin: rule.EditDemandAll},
			presentation,
		))
		if err != nil {
			t.Fatal(err)
		}
		if presentation.run == nil || !released || !presented {
			t.Fatalf("run/released/presented = %v/%v/%v", presentation.run != nil, released, presented)
		}
		if _, complete := result.Observation.CompleteDiagnostics(); complete {
			t.Fatal("progressive result reported complete before enrichment")
		}
	})

	for _, test := range []struct {
		name         string
		text         string
		pluginConfig *EslintPluginFileConfig
		rules        []rule.ConfiguredRule
	}{
		{
			name:         "target syntax error",
			text:         "const value = ;",
			pluginConfig: &EslintPluginFileConfig{},
			rules:        []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		},
		{name: "no plugin work", text: "const value = 1;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := tspath.NormalizePath(t.TempDir())
			fileName := tspath.ResolvePath(root, "source.ts")
			generation := pipelineTestGeneration(t, root, fileName, test.text, test.rules, test.pluginConfig)
			presentation := &pipelineProgressiveDiagnostics{}
			result, err := RunPipeline(context.Background(), NewProgressiveLintRequest(
				pipelineTestProvider(generation, nil),
				ArtifactDemand{Plugin: rule.EditDemandAll},
				presentation,
			))
			if err != nil {
				t.Fatal(err)
			}
			if presentation.run != nil {
				t.Fatal("ineligible enrichment was submitted")
			}
			if _, complete := result.Observation.CompleteDiagnostics(); !complete {
				t.Fatal("baseline without enrichment was reported incomplete")
			}
		})
	}
}

func TestConcurrentPipelineChecksCancellationAfterRelease(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(t, root, fileName, "a", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result, err := RunPipeline(ctx, NewLintRequest(
		pipelineTestProvider(generation, ReleaseFunc(cancel)),
		ObservationPolicy{Plugin: PluginConcurrentJoined},
		nil,
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipeline error = %v, want cancellation raised by release", err)
	}
	if result.Observation.Native.Lint != nil {
		t.Fatal("post-release canceled observation was published")
	}
}

func TestPipelineAfterNativeReleasesBeforePluginDispatch(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		&EslintPluginFileConfig{},
	)
	var releases atomic.Int32
	_, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, func() { releases.Add(1) }),
		ObservationPolicy{
			Demand:        ArtifactDemand{Plugin: rule.EditDemandAll},
			Plugin:        PluginAfterNativeJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		func(_ context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			if releases.Load() != 1 {
				t.Fatalf("release count at plugin dispatch = %d, want 1", releases.Load())
			}
			return &EslintPluginLintResult{Results: []EslintPluginFileResult{{FilePath: request.Files[0].Path}}}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", releases.Load())
	}
}

func TestProgressivePluginRunIsFrozenAndSingleUse(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	settings := map[string]any{"value": "frozen"}
	options := []any{map[string]any{"choice": "frozen"}}
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"const value = 1;",
		[]rule.ConfiguredRule{{
			Name:               "plugin/check",
			IsEslintPluginRule: true,
			Options:            options,
		}},
		&EslintPluginFileConfig{Settings: settings},
	)
	var releases atomic.Int32
	presentation := &pipelineProgressiveDiagnostics{}
	_, err := RunPipeline(context.Background(), NewProgressiveLintRequest(
		pipelineTestProvider(generation, func() { releases.Add(1) }),
		ArtifactDemand{Plugin: rule.EditDemandAll},
		presentation,
	))
	if err != nil {
		t.Fatal(err)
	}
	if presentation.run == nil || releases.Load() != 1 {
		t.Fatalf("enrichment/release = %v/%d, want non-nil/1", presentation.run != nil, releases.Load())
	}
	settings["value"] = "mutated"
	options[0].(map[string]any)["choice"] = "mutated"
	var request EslintPluginLintRequest
	outcome, err := presentation.run(context.Background(), func(_ context.Context, got EslintPluginLintRequest) (*EslintPluginLintResult, error) {
		request = got
		return &EslintPluginLintResult{Results: []EslintPluginFileResult{{FilePath: got.Files[0].Path}}}, nil
	})
	if err != nil || outcome.DispatchError != nil {
		t.Fatalf("work errors = %v/%v", err, outcome.DispatchError)
	}
	frozenOptions := request.Rules["plugin/check"].Options
	if request.Files[0].Settings["value"] != "frozen" ||
		len(frozenOptions) != 1 || frozenOptions[0].(map[string]any)["choice"] != "frozen" {
		t.Fatalf("deferred request retained mutable config: %+v", request)
	}
	if _, err := presentation.run(context.Background(), func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
		return &EslintPluginLintResult{}, nil
	}); !errors.Is(err, ErrDeferredPluginRunAlreadyInvoked) {
		t.Fatalf("second run error = %v, want ErrDeferredPluginRunAlreadyInvoked", err)
	}
}

func TestConcurrentPipelineCancelsAndJoinsPluginBeforeReleaseOnNativePanic(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	pluginStarted := make(chan struct{})
	pluginStopped := make(chan struct{})
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{
			{
				Name: "native/panic",
				Run: func(rule.RuleContext) rule.RuleListeners {
					<-pluginStarted
					panic("native failed")
				},
			},
			{Name: "plugin/check", IsEslintPluginRule: true},
		},
		&EslintPluginFileConfig{},
	)
	var releases atomic.Int32
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = RunPipeline(context.Background(), NewLintRequest(
			pipelineTestProvider(generation, func() {
				select {
				case <-pluginStopped:
				default:
					t.Fatal("generation released before plugin dispatch joined")
				}
				releases.Add(1)
			}),
			ObservationPolicy{Plugin: PluginConcurrentJoined},
			func(pluginCtx context.Context, _ EslintPluginLintRequest) (*EslintPluginLintResult, error) {
				close(pluginStarted)
				<-pluginCtx.Done()
				close(pluginStopped)
				return nil, pluginCtx.Err()
			},
		))
	}()
	if recovered == nil || releases.Load() != 1 {
		t.Fatalf("panic/releases = %v/%d, want panic/1", recovered, releases.Load())
	}
}

type pipelineAutofixProvider struct {
	t            *testing.T
	root         string
	fileName     string
	initial      string
	next         map[string]string
	acquisitions int
	observed     []string
	rules        func(string) []rule.ConfiguredRule
}

func (p *pipelineAutofixProvider) AcquireGeneration(
	_ context.Context,
	snapshot SourceSnapshot,
) (Generation, ReleaseFunc, error) {
	p.acquisitions++
	current := p.initial
	files := snapshot.Files()
	if len(files) > 0 {
		if len(files) != 1 || files[0].Path != p.fileName {
			return Generation{}, nil, errors.New("unexpected source snapshot")
		}
		current = files[0].Text
	}
	p.observed = append(p.observed, current)
	configuredRules := []rule.ConfiguredRule{}
	if next, ok := p.next[current]; ok {
		before := current
		configuredRules = append(configuredRules, rule.ConfiguredRule{
			Name:     "native/fix",
			Severity: rule.SeverityError,
			Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
				rangeToFix := core.NewTextRange(0, len(before))
				ruleCtx.ReportRangeWithFixes(
					rangeToFix,
					rule.RuleMessage{Description: "fix"},
					rule.RuleFix{Range: rangeToFix, Text: next},
				)
				return nil
			},
		})
	}
	if p.rules != nil {
		configuredRules = append(configuredRules, p.rules(current)...)
	}
	var pluginConfig *EslintPluginFileConfig
	for _, configured := range configuredRules {
		if configured.IsEslintPluginRule {
			pluginConfig = &EslintPluginFileConfig{}
			break
		}
	}
	return pipelineTestGeneration(p.t, p.root, p.fileName, current, configuredRules, pluginConfig), nil, nil
}

type pipelineFinalChangeRecorder struct {
	commits   int
	committed [][]FileChange
	commit    func(context.Context, []FileChange) (CommitResult, error)
}

func (r *pipelineFinalChangeRecorder) CommitFinalChanges(
	ctx context.Context,
	changes []FileChange,
) (CommitResult, error) {
	r.commits++
	r.committed = append(r.committed, cloneFileChanges(changes))
	if r.commit != nil {
		return r.commit(ctx, changes)
	}
	paths := make([]string, len(changes))
	for index, change := range changes {
		paths[index] = change.Path
	}
	return CommitResult{ConfirmedPaths: paths}, nil
}

func TestAutofixPipelineOwnsMemoryRoundsAndReobservesSnapshots(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "a",
		next:     map[string]string{"a": "b", "b": "c"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: MaxFixRounds},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 2 ||
		provider.acquisitions != 3 || strings.Join(provider.observed, ",") != "a,b,c" {
		t.Fatalf("result/provider = %+v / %+v", applied, provider)
	}
	if len(applied.FinalChanges) != 1 || applied.FinalChanges[0].Before != "a" || applied.FinalChanges[0].After != "c" {
		t.Fatalf("final in-memory delta = %+v", applied.FinalChanges)
	}
	if applied.Rounds[0].AppliedDiagnostics != 1 || applied.Rounds[1].AppliedDiagnostics != 1 {
		t.Fatalf("applied diagnostic counts = %+v", applied.Rounds)
	}
}

func TestCommittedAutofixCommitsOnlyFinalDeltaOnce(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "a",
		next:     map[string]string{"a": "b", "b": "c"},
	}
	committer := &pipelineFinalChangeRecorder{}
	committer.commit = func(_ context.Context, changes []FileChange) (CommitResult, error) {
		if provider.acquisitions != 3 || strings.Join(provider.observed, ",") != "a,b,c" {
			t.Fatalf("terminal commit ran before all observations: %+v", provider)
		}
		return CommitResult{ConfirmedPaths: []string{changes[0].Path}}, nil
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequestWithCommitter(
		provider,
		committer,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: MaxFixRounds},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Committed || committer.commits != 1 || len(committer.committed) != 1 || len(committer.committed[0]) != 1 {
		t.Fatalf("committed result/committer = %+v / %+v", applied, committer)
	}
	change := committer.committed[0][0]
	if change.Before != "a" || change.After != "c" || change.AppliedDiagnostics != 2 {
		t.Fatalf("terminal delta = %+v", change)
	}
}

func TestAutofixSyntaxGateStopsBeforeAfterNativePluginDispatch(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "const value = ;",
		next:     map[string]string{},
		rules: func(string) []rule.ConfiguredRule {
			return []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
		},
	}
	dispatches := 0
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{
			Demand: ArtifactDemand{
				Native: rule.EditDemandAutofix,
				Plugin: rule.EditDemandAutofix,
			},
			Plugin:        PluginAfterNativeJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		AutofixPolicy{MaxRounds: MaxFixRounds, StopOnTargetSyntaxErrors: true},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			dispatches++
			return &EslintPluginLintResult{}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 0 || !result.Observation.Native.HasTargetSyntaxErrors {
		t.Fatalf("syntax-gated applied result = %+v", applied)
	}
	if dispatches != 0 {
		t.Fatalf("plugin dispatches after target syntax error = %d, want 0", dispatches)
	}
}

func TestAutofixSyntaxGateStopsConcurrentPluginBeforeDispatch(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	brokenPath := tspath.ResolvePath(root, "broken.ts")
	pluginPath := tspath.ResolvePath(root, "plugin.ts")
	generation := Generation{
		Native: NativeGeneration{
			Programs: []*program.Program{
				pipelineTestProgram(t, root, brokenPath, "const value = ;"),
				pipelineTestProgram(t, root, pluginPath, "const value = 1;"),
			},
			TargetsByProgram: [][]string{{brokenPath}, {pluginPath}},
			SingleThreaded:   true,
			RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
				if source.FileName() != pluginPath {
					return nil
				}
				return []rule.ConfiguredRule{{Name: "plugin/fix", IsEslintPluginRule: true}}
			},
		},
		Target: TargetProjection{ReadText: func(_ string, source ast.SourceFileLike) (string, error) {
			return source.Text(), nil
		}},
		Plugin: &PluginGeneration{},
	}
	dispatches := 0
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{
			Demand: ArtifactDemand{Plugin: rule.EditDemandAutofix},
			Plugin: PluginConcurrentJoined,
		},
		AutofixPolicy{MaxRounds: 1, StopOnTargetSyntaxErrors: true},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			dispatches++
			return &EslintPluginLintResult{}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 0 ||
		!result.Observation.Native.HasTargetSyntaxErrors || dispatches != 0 {
		t.Fatalf("syntax-gated result/dispatches = %+v/%d", applied, dispatches)
	}
}

func TestAutofixUsesProductRoundLimitThenVerifiesOnce(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	next := make(map[string]string, MaxFixRounds)
	for round := range MaxFixRounds {
		next[strconv.Itoa(round)] = strconv.Itoa(round + 1)
	}
	var fixArtifactBuilds atomic.Int32
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "0",
		next:     next,
		rules: func(string) []rule.ConfiguredRule {
			return []rule.ConfiguredRule{{
				Name: "native/demand-probe",
				Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
					probeRange := core.NewTextRange(0, 0)
					ruleCtx.ReportRangeWithDeferredFixes(
						probeRange,
						rule.RuleMessage{Description: "probe"},
						func() []rule.RuleFix {
							fixArtifactBuilds.Add(1)
							return nil
						},
					)
					return nil
				},
			}}
		},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{
			MaxRounds:            MaxFixRounds,
			VerifyAfterLastRound: true,
			VerificationDemand: ArtifactDemand{
				Native:      rule.EditDemandSuggestion,
				LintedFiles: true,
			},
		},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != MaxFixRounds || applied.Last.Index != MaxFixRounds {
		t.Fatalf("applied result = %+v", applied)
	}
	if applied.Initial.Native.Files != nil || len(applied.Last.Native.Files) != 1 {
		t.Fatalf(
			"per-observation linted-file demand = initial:%+v verification:%+v",
			applied.Initial.Native.Files,
			applied.Last.Native.Files,
		)
	}
	if provider.acquisitions != MaxFixRounds+1 || provider.observed[len(provider.observed)-1] != strconv.Itoa(MaxFixRounds) {
		t.Fatalf("provider acquisitions/observations = %d/%+v", provider.acquisitions, provider.observed)
	}
	if fixArtifactBuilds.Load() != MaxFixRounds {
		t.Fatalf("autofix artifact builds = %d, want %d; final verification demand was not isolated", fixArtifactBuilds.Load(), MaxFixRounds)
	}
}

func TestAutofixCanStopAtRoundLimitWithoutVerification(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "a",
		next:     map[string]string{"a": "b", "b": "c", "c": "d"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 2},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || len(applied.Rounds) != 2 || applied.Last.Index != 1 {
		t.Fatalf("unverified applied result = %+v", applied)
	}
	if provider.acquisitions != 2 || strings.Join(provider.observed, ",") != "a,b" ||
		len(applied.FinalChanges) != 1 || applied.FinalChanges[0].After != "c" {
		t.Fatalf("provider/result = %+v / %+v", provider, applied)
	}
}

func TestAutofixTerminalCommitErrorReturnsConfirmedExternalState(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	paths := [2]string{tspath.ResolvePath(root, "first.ts"), tspath.ResolvePath(root, "second.ts")}
	initial := map[string]string{paths[0]: "a", paths[1]: "b"}
	acquisitions := 0
	provider := GenerationProviderFunc(func(_ context.Context, snapshot SourceSnapshot) (Generation, ReleaseFunc, error) {
		acquisitions++
		texts := map[string]string{paths[0]: initial[paths[0]], paths[1]: initial[paths[1]]}
		for _, file := range snapshot.Files() {
			texts[file.Path] = file.Text
		}
		programs := []*program.Program{
			pipelineTestProgram(t, root, paths[0], texts[paths[0]]),
			pipelineTestProgram(t, root, paths[1], texts[paths[1]]),
		}
		return Generation{
			Native: NativeGeneration{
				Programs:         programs,
				TargetsByProgram: [][]string{{paths[0]}, {paths[1]}},
				SingleThreaded:   true,
				Cwd:              root,
				RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
					return []rule.ConfiguredRule{{Name: "native/fix", Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
						textRange := core.NewTextRange(0, len(source.Text()))
						ruleCtx.ReportRangeWithFixes(textRange, rule.RuleMessage{Description: "fix"}, rule.RuleFix{Range: textRange, Text: "fixed"})
						return nil
					}}}
				},
			},
			Target: TargetProjection{ReadText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			}},
		}, nil, nil
	})
	committer := &pipelineFinalChangeRecorder{
		commit: func(_ context.Context, changes []FileChange) (CommitResult, error) {
			return CommitResult{ConfirmedPaths: []string{changes[0].Path}}, errors.New("partial commit")
		},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequestWithCommitter(
		provider,
		committer,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "partial commit") {
		t.Fatalf("apply error = %v", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || applied.Committed || len(applied.Rounds) != 1 ||
		len(applied.CommittedPaths) != 1 || applied.CommittedPaths[0] != paths[0] ||
		applied.Rounds[0].AppliedDiagnostics != 2 {
		t.Fatalf("partial applied result = %+v", applied)
	}
	if acquisitions != 1 || committer.commits != 1 {
		t.Fatalf("acquisitions/commits = %d/%d, want 1/1", acquisitions, committer.commits)
	}
}

func TestAutofixPropagatesCancellationFromTerminalCommitter(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	commitErr := errors.New("commit failed")
	provider := &pipelineAutofixProvider{
		t: t, root: root, fileName: tspath.ResolvePath(root, "source.ts"), initial: "a", next: map[string]string{"a": "b"},
	}
	committer := &pipelineFinalChangeRecorder{}
	committer.commit = func(context.Context, []FileChange) (CommitResult, error) {
		cancel()
		return CommitResult{}, commitErr
	}
	result, err := RunPipeline(ctx, NewAutofixRequestWithCommitter(
		provider,
		committer,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), commitErr.Error()) {
		t.Fatalf("pipeline error = %v, want joined commit and cancellation errors", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || applied.Committed || len(applied.Rounds) != 1 || len(applied.FinalChanges) != 1 {
		t.Fatalf("applied result = %+v", applied)
	}
}

func TestAutofixRestoredInitialReusesInitialObservation(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "a",
		next:     map[string]string{"a": "b", "b": "a"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 3},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 2 || !applied.Rounds[1].RestoredInitial ||
		applied.Last.Index != applied.Initial.Index || result.Observation.Index != 0 || len(applied.FinalChanges) != 0 {
		t.Fatalf("restored applied result/provider = %+v / %+v", applied, provider)
	}
	if provider.acquisitions != 2 || strings.Join(provider.observed, ",") != "a,b" {
		t.Fatalf("restored cycle provider = %+v", provider)
	}
}

func TestAutofixSameTextFixConsumesRound(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "a",
		next:     map[string]string{"a": "a"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 1 || len(applied.FinalChanges) != 0 ||
		len(applied.Rounds[0].ChangedPaths) != 1 || applied.Rounds[0].AppliedDiagnostics != 1 {
		t.Fatalf("same-text applied result/provider = %+v / %+v", applied, provider)
	}
}

func TestAutofixRejectsRoundLimitAboveProductBound(t *testing.T) {
	provider := &pipelineAutofixProvider{}
	_, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: MaxFixRounds + 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "product safety bound") {
		t.Fatalf("round bound error = %v", err)
	}
	if provider.acquisitions != 0 {
		t.Fatalf("invalid request acquired %d generations", provider.acquisitions)
	}
}

func TestCommittedAutofixRejectsNilCommitterBeforeAcquisition(t *testing.T) {
	provider := &pipelineAutofixProvider{}
	_, err := RunPipeline(context.Background(), NewAutofixRequestWithCommitter(
		provider,
		nil,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "committer must not be nil") {
		t.Fatalf("nil committer error = %v", err)
	}
	if provider.acquisitions != 0 {
		t.Fatalf("invalid request acquired %d generations", provider.acquisitions)
	}
}

func TestAutofixPipelineRejectsFalseTerminalCommitConfirmation(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	provider := &pipelineAutofixProvider{
		t: t, root: root, fileName: tspath.ResolvePath(root, "source.ts"), initial: "a", next: map[string]string{"a": "b"},
	}
	committer := &pipelineFinalChangeRecorder{
		commit: func(context.Context, []FileChange) (CommitResult, error) { return CommitResult{}, nil },
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequestWithCommitter(
		provider,
		committer,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "confirmed 0 of 1") {
		t.Fatalf("commit contract error = %v", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || len(applied.Rounds) != 1 {
		t.Fatalf("partial contract result = %+v", applied)
	}
}

func TestAutofixReobserveFailurePreservesLastSuccessfulObservation(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	nativeFinished := make(chan struct{})
	provider := &pipelineAutofixProvider{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		initial:  "a",
		next:     map[string]string{"a": "b"},
		rules: func(content string) []rule.ConfiguredRule {
			rules := []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
			if content == "b" {
				rules = append(rules, rule.ConfiguredRule{
					Name: "native/second-only",
					Run: func(rule.RuleContext) rule.RuleListeners {
						close(nativeFinished)
						return nil
					},
				})
			}
			return rules
		},
	}
	committer := &pipelineFinalChangeRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dispatches atomic.Int32
	result, err := RunPipeline(ctx, NewAutofixRequestWithCommitter(
		provider,
		committer,
		ObservationPolicy{
			Demand: ArtifactDemand{
				Native: rule.EditDemandAutofix,
				Plugin: rule.EditDemandAutofix,
			},
			Plugin:        PluginConcurrentJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		AutofixPolicy{MaxRounds: 1, VerifyAfterLastRound: true},
		func(dispatchCtx context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			if dispatches.Add(1) == 2 {
				<-nativeFinished
				cancel()
				return nil, dispatchCtx.Err()
			}
			return &EslintPluginLintResult{Results: []EslintPluginFileResult{{
				FilePath: request.Files[0].Path,
			}}}, nil
		},
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipeline error = %v, want context.Canceled", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || applied.Last.Index != 0 || result.Observation.Index != 0 ||
		len(applied.FinalChanges) != 1 || applied.FinalChanges[0].After != "b" || committer.commits != 0 {
		t.Fatalf("applied result = %+v, observation=%+v", applied, result.Observation)
	}
	records := result.PluginOutcomes()
	if len(records) != 2 || records[1].Observation != 1 || !errors.Is(records[1].DispatchError, context.Canceled) {
		t.Fatalf("plugin records = %+v, want failed observation 1 retained", records)
	}
	if _, leaked := result.ExecutedRules()["native/second-only"]; leaked {
		t.Fatal("failed re-observation polluted successful rule aggregation")
	}
}

func TestCommitResultRejectsInvalidExternalConfirmations(t *testing.T) {
	planned := []FileChange{{Path: "a.ts", Before: "a", After: "b"}}
	tests := []struct {
		name   string
		result CommitResult
		err    error
	}{
		{name: "partial without error", result: CommitResult{}},
		{name: "complete with error", result: CommitResult{ConfirmedPaths: []string{"a.ts"}}, err: errors.New("commit failed")},
		{name: "extra confirmation", result: CommitResult{ConfirmedPaths: []string{"a.ts", "extra.ts"}}},
		{name: "duplicate confirmation", result: CommitResult{ConfirmedPaths: []string{"a.ts", "a.ts"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			confirmed, err := validateCommitResult(planned, test.result, test.err)
			if err == nil {
				t.Fatal("invalid terminal confirmation was accepted")
			}
			if test.name == "partial without error" && len(confirmed) != 0 {
				t.Fatalf("unexpected confirmed paths: %+v", confirmed)
			}
		})
	}
}

func TestAutofixRejectsProviderThatIgnoresMemorySnapshot(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(t, root, fileName, "a", []rule.ConfiguredRule{{
		Name: "native/fix",
		Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
			textRange := core.NewTextRange(0, 1)
			ruleCtx.ReportRangeWithFixes(textRange, rule.RuleMessage{Description: "fix"}, rule.RuleFix{Range: textRange, Text: "b"})
			return nil
		},
	}}, nil)
	acquisitions := 0
	provider := GenerationProviderFunc(func(context.Context, SourceSnapshot) (Generation, ReleaseFunc, error) {
		acquisitions++
		return generation, nil, nil
	})
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		provider,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 2},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "did not materialize in-memory target") {
		t.Fatalf("stale generation error = %v", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || acquisitions != 2 || len(applied.FinalChanges) != 1 || applied.FinalChanges[0].After != "b" {
		t.Fatalf("stale provider result/acquisitions = %+v/%d", applied, acquisitions)
	}
}

func TestPipelineFreezesTextOnlyForFixableTargets(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fixablePath := tspath.ResolvePath(root, "fixable.ts")
	nonFixablePath := tspath.ResolvePath(root, "clean.ts")
	fixableProgram := pipelineTestProgram(t, root, fixablePath, "a")
	nonFixableProgram := pipelineTestProgram(t, root, nonFixablePath, "clean")
	generation := Generation{
		Native: NativeGeneration{
			Programs:         []*program.Program{fixableProgram, nonFixableProgram},
			TargetsByProgram: [][]string{{fixablePath}, {nonFixablePath}},
			SingleThreaded:   true,
			Cwd:              root,
			RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
				if source.FileName() != fixablePath {
					return nil
				}
				return []rule.ConfiguredRule{{
					Name: "native/fix",
					Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
						textRange := core.NewTextRange(0, 1)
						ruleCtx.ReportRangeWithFixes(
							textRange,
							rule.RuleMessage{Description: "fix"},
							rule.RuleFix{Range: textRange, Text: "b"},
						)
						return nil
					},
				}}
			},
		},
		Target: TargetProjection{
			ReadText: func(path string, source ast.SourceFileLike) (string, error) {
				if path != fixablePath {
					return "", fmt.Errorf("unexpected fix text read for %q", path)
				}
				return source.Text(), nil
			},
		},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	changes := applied.FinalChanges
	if !ok || len(changes) != 1 || changes[0].Path != fixablePath {
		t.Fatalf("planned changes = %+v, planned=%v", changes, ok)
	}
}

func TestPlanFixesIsPureAndDeterministic(t *testing.T) {
	unsortedFixes := []rule.RuleFix{
		{Range: core.NewTextRange(1, 2), Text: "Z"},
		{Range: core.NewTextRange(0, 1), Text: "A"},
	}
	diagnostics := []rule.RuleDiagnostic{
		{
			FilePath: "z.ts",
			Range:    core.NewTextRange(0, 1),
			FixesPtr: &[]rule.RuleFix{{Range: core.NewTextRange(0, 1), Text: "Z"}},
		},
		{
			FilePath: "a.ts",
			Range:    core.NewTextRange(0, 2),
			FixesPtr: &unsortedFixes,
		},
	}
	changes, err := planFixes(diagnostics, fixTextSnapshot{"z.ts": "z", "a.ts": "ab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Path != "a.ts" || changes[0].After != "AZ" || changes[1].Path != "z.ts" {
		t.Fatalf("changes = %+v", changes)
	}
	if unsortedFixes[0].Range.Pos() != 1 || unsortedFixes[1].Range.Pos() != 0 {
		t.Fatalf("planner mutated caller-owned fixes: %+v", unsortedFixes)
	}
}
