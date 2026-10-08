# Benchmarks

Every script needs Docker and Go. The agent benchmarks also need the `claude`
CLI and `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`. See CONTRIBUTING.md
for when a change MUST come with a result.

## Supported benchmarks

### Agent benchmarks

`run.py` runs Claude on each task, once per variant, and grades the answer.
A dataset script turns its dataset into a tasks file for `run.py --tasks-file`.

| Benchmark | Script | Measures | Graded by |
|---|---|---|---|
| keepsake tasks | `tasks.json` | Reading, linking and writing on the demo bundle in `bundle/` | Regexes, reads and store diffs |
| LongMemEval | `datasets/longmemeval.py` | Memory of a user over ~50 (S) or ~500 (M) chat sessions | LongMemEval's judge |
| LoCoMo | `datasets/locomo.py` | Memory of two people over up to 35 sessions | Mem0's J-score judge |
| BEAM | `datasets/beam.py` | Memory over 100K to 10M tokens of conversation | BEAM's rubric judge |
| MuSiQue | `datasets/musique.py` | 2–4 hop questions, with and without links | Answer aliases |
| IIRC | `datasets/iirc.py` | Questions whose answer sits in a linked article | Answer aliases |

### Search benchmarks

No agent: these score `search`'s ranking directly.

| Benchmark | Script | Measures |
|---|---|---|
| BEIR | `datasets/beir.py` | nDCG@10 and recall on SciFact, NFCorpus and FiQA |
| Rankers | `rankers.py` | Rankers in `rankers.sql` on BEIR and LongMemEval, and their latency at 100k concepts |

`benchmarks.pdf` holds the current results.

## Layout

| Path | Holds |
|---|---|
| `run.py`, `grade.py` | The agent harness and its grading |
| `datasets/` | One script per external dataset. Each docstring gives its license and commands. |
| `variants/` | Prompt and tool variants for `run.py --variants` |
| `tasks.json`, `bundle/` | keepsake's own tasks and the bundle they run on |
| `rankers.py`, `rankers.sql` | The ranker comparison |
| `data/`, `results/` | Downloads and run output. Not committed. |

## Variants

| Variant | Agent reads memory through |
|---|---|
| `baseline` | keepsake's tools, as this tree ships them |
| `main` | keepsake's tools, as `origin/main` ships them |
| `files` | Claude Code's Read, Grep and Glob over an exported snapshot |
| `bash` | A sandboxed Bash over an exported snapshot. It reads nothing outside the snapshot and has no network. |
| `monolith` | Nothing: the whole bundle is in the prompt |
| `no-grep`, `no-instructions`, `terse-tools`, `team-instructions`, `thorough` | keepsake's tools with one thing changed, named in the file's `description` |

## Running one

```bash
python3 bench/datasets/locomo.py --conversations 3
python3 bench/run.py --tasks-file bench/data/locomo/agent/locomo.json --variants baseline bash --trials 1
python3 bench/run.py --report bench/results/<run>
```
