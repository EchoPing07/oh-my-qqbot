# internal/memorix — A_memorix 长期记忆内核（Go 重写）

对齐 `参考/MaiBot/src/A_memorix/`（Python 134 文件 / 7.1 万行），按
`docs/REWRITE_PLAN.md` §6.14 重写为纯 Go + SQLite（`modernc.org/sqlite`，无 CGO）。

## 分层

```
memorix/
├── memorix.go          Options / EmbeddingOptions / ExtractionOptions + 最小 LLM 接口
├── kernel.go           Kernel：实现 internal.MemoryKernel（SearchMemory 五模式）
├── embed.go            EmbeddingClient：维度探测 / profile 缓存 / 失败降级
├── extract.go          LLM 抽取 + 宽松 JSON 修复 + relation hash 去重
├── retrieval/          BM25 · Aho-Corasick · PPR · 双路融合 · 后验图门
└── storage/            SQLite schema · FTS5 · 向量表 + 内存 flat 内积索引
```

依赖方向单向向内：`kernel → retrieval → storage`，`storage` 只依赖 `retrieval` 的
数据结构。**不** import `internal/chat` / `internal/qq` / `internal/config`；
配置通过 `Options` 结构体注入（装配侧负责从 `internal/config` 转换）。

## 存储层（单文件 `data/a-memorix/a_memorix.db`）

| 表 | 对齐出处（Python） |
| --- | --- |
| `paragraphs` | `core/storage/metadata_schema.py` L1264-L1284 |
| `entities` | 同上 L1291-L1301 |
| `relations` | 同上 L1305-L1337 |
| `paragraph_links` | 同上 L1391-L1397（Python 的 `paragraph_relations`） |
| `entity_links` | 同上 L1402-L1410（Python 的 `paragraph_entities`） |
| `episodes` / `episode_paragraphs` | 同上 L1530-L1563 |
| `profiles` / `profile_facts` | L1471-L1486（`person_profile_snapshots`）+ `metadata_fact.py` L30-L51（`fact_claims`） |
| `paragraphs_fts`（external-content） | `core/storage/metadata_fts.py` L34-L113 |
| `relations_fts`（独立表） | 同上 L115-L198 |
| `paragraphs_tokenized_fts`（预分词 shadow） | 同上 L199-L311 |
| `vectors`（BLOB float32） | Go 新增（Python 用 faiss 文件） |
| `external_refs`（幂等映射） | `external_memory_refs` |

> **关键点**：SQLite FTS5 的 `unicode61` 分词器会把整句中文当作一个 token，
> 因此 Python 版的主 BM25 路径实际是 `paragraphs_tokenized_fts`（写入时用
> jieba + 短语正则预分词）。Go 版照抄该结构与回填逻辑（`storage/fts.go`）。

向量检索：写入时 L2 归一化（`vectors.norm` 留档），内存中按 `item_type` 分池做
flat 内积 TopK（归一化后即余弦）；启动时从 `vectors` 表重建（`loadVectorIndex`）。

## 检索层

| 模块 | 对齐出处（Python） |
| --- | --- |
| `bm25.go` | `core/retrieval/sparse_bm25.py`（jieba→gse、MATCH 表达式、尾部裁剪） |
| `ac_matcher.go` | `core/utils/matcher.py`（goto 表 + fail 指针 + 输出集合） |
| `pagerank.go` | `core/retrieval/pagerank.py` + `graph_store.py` L1111-L1195 |
| `dual_path.go` | `core/retrieval/dual_path.py`（双向量池主链 + 经典双路 + 时序） |
| `fuse.go` | `core/retrieval/score_calibration.py` |
| `posterior.go` | `core/retrieval/posterior_graph.py` |
| `threshold.go` | `core/retrieval/threshold.py`（adaptive 阈值） |
| `constants.go` | 全部照抄常数的集中登记（逐条标注文件 + 行号） |

## SearchMemory 模式路由

| mode | 行为 | 出处 |
| --- | --- | --- |
| `search` | 语义召回 + 三通道融合（双向量池） | `dual_path.py` `_retrieve_dual_vector_pools` |
| `time` | 时序扫描（默认最近 24h，query 为空时跳过阈值过滤） | `_retrieve_temporal_only` |
| `hybrid` | 语义召回 + 时间窗过滤（解析「今天/昨天/最近 N 天」等） | `SearchExecutionService._build_temporal` |
| `episode` | episode 词法检索（title/summary/keywords/participants） | `metadata_episode.py` L1505-L1557 |
| `aggregate` | search + time + episode 三路 RRF 混合 | `aggregate_query_service.py` L108-L158 |

`top_k` 裁剪对齐 `_resolve_top_k`（time/hybrid 用 `temporal.default_top_k`，其余默认 5，
统一裁剪到 `[1, 50]`）。

## 嵌入降级

`EmbeddingClient` 对齐 `core/embedding/api_adapter.py`：

- 维度探测：首次成功响应确定维度；未探测前用配置维度预置但不标记「已探测」。
- profile 缓存：`embedding.enable_cache` 开启时按文本缓存向量。
- 失败降级：`embedding.fallback.allow_metadata_only_write`（默认 true）允许只在
  `paragraphs`/`entities`/`relations` 落元数据、跳过向量写入；关闭时写入直接报错。

## 测试

```bash
go test ./internal/memorix/...        # 全绿
go test ./internal/memorix/... -cover
```

覆盖点：

- `retrieval/constants_test.go`：逐条断言 Python 常数（含文件 + 行号出处），
  grounding 四级衰减、图可靠性三项、图权重曲线缩放、按类型 min-max。
- `retrieval/retrieval_test.go`：BM25 单调性 / MATCH 转义 / 尾部裁剪 / auto 回落阈值、
  AC 重叠匹配与位置、PPR 收敛与个性化与悬挂节点与局部子图、两跳路径、融合权重与
  alpha 加权与去重、关系意图、阈值过滤、分词模式。
- `storage/storage_test.go`：建表与 FTS5 可用性、段落/关系 FTS round-trip、
  relation hash 规范化去重、向量 BLOB 编解码与重启重建、余弦排序与 scope、
  链接表回表、图构建、时序查询、画像事实、episode 检索。
- `memorix/kernel_test.go` + `integration_test.go`：JSON 修复五种形态、抽取降级、
  嵌入降级（允许/禁止 metadata-only）、维度探测、五种检索模式、会话隔离、
  ingest 幂等、图片记忆幂等、人物画像与关系、并发读写、双向量池端到端。
