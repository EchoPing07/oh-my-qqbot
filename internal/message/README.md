# internal/message — P2 work package

组件化消息模型（替代 MaiBot 的 msgpack 持久化，`raw_content` 改 JSON）。

## 文件 → 对齐源

| 本包文件 | 对齐的 MaiBot 源 |
| --- | --- |
| `components.go` | `src/common/data_models/message_component_data_model.py`（Text/Image/Emoji/Voice/File/At/Reply/Forward/ForwardNode/Dict + ByteComponent）<br>`src/chat/message_receive/message.py`（各组件 → 文本的渲染规则） |
| `sequence.go` | `MessageSequence`（to_dict/from_dict、链式 builder、占位符）<br>`src/common/utils/utils_message.py`（`MessageUtils.from_MaiSeq_to_db_record_msg` / `from_db_record_msg_to_MaiSeq`，msgpack→JSON）<br>`src/chat/utils/utils.py::assign_message_ids` |
| `session_message.go` | `src/common/data_models/mai_message_data_model.py`（UserInfo/GroupInfo/MessageInfo/MaiMessage，`to_db_instance`/`from_db_instance`）<br>`src/chat/message_receive/message.py`（`SessionMessage.process` / `to_debug_string`） |
| `session_id.go` | `src/common/utils/utils_session.py::SessionUtils.calculate_session_id`<br>`src/platform_io/route_key_factory.py`（account_id/scope 键优先级） |

> 注：按 P2 任务要求先在 `参考/MaiBot/src/chat/message_receive/` 下 grep `calculate_session_id`，
> 该目录内只有**调用点**（`bot.py:759`、`chat_manager.py:118/188/353`），定义在
> `src/common/utils/utils_session.py:8`。四处调用都是
> `calculate_session_id(platform, user_id=…, group_id=…, account_id=…, scope=…)`，
> 本包 `CalculateSessionIDStrings` 的参数顺序与之完全一致，`SessionIDForMessage`
> 复刻 `register_message` 从 `additional_config` 提取 route 元数据的路径。

## session_id 算法（逐字节对齐）

```python
route_components = []
if account_id: route_components.append(f"account:{account_id}")
if scope:      route_components.append(f"scope:{scope}")
if group_id:   components = [platform, *route_components, group_id]
else:          components = [platform, *route_components, user_id, "private"]
session_id = hashlib.md5("_".join(components).encode()).hexdigest()
```

Go 侧 `CalculateSessionID` 保持相同的分支与拼接顺序：

* 分隔符是单个下划线；私聊的 `private` 本身就是最后一个组件，因此**没有**尾随下划线。
* 空字符串按 Python 真值语义处理：`account_id`/`scope` 为空则**完全不产生**组件（不留多余下划线）；
  `group_id` 为空才走私聊分支；两者都空返回 `ErrMissingSessionIdentity`。
* 摘要为 UTF-8 字节的小写 hex md5。
* `group_id` 与 `user_id` 同时存在时群聊分支优先。

测试 `TestCalculateSessionIDMatchesPython` 直接钉住 Python `hashlib.md5` 的输出向量
（如 `qq_g1` → `md5("qq_g1")`、`qq_account:bot1_123456`、`qq_scope:routeA_123456`）。

`SessionRouteComponents` 复刻 `RouteKeyFactory.extract_components`：按
`platform_io_account_id → account_id → self_id → bot_account` 与
`platform_io_scope → route_scope → adapter_scope → connection_id` 取第一个非空值。

## 设计要点

* `Component` 接口 = Python 的 `format_name` + `clone`；序列化用各组件自定义的
  `MarshalJSON`，输出与 Python `_item_2_dict` 相同的 `{"type":..,"data":..,"hash":..}` 形状。
* 未知组件类型降级为 `DictComponent`（Python 记 warning 后同样降级）。
* 二进制组件（image/emoji/voice）序列化时按 Python 补稳定占位符
  `[表情包]` / `[语音消息]`；`_dict_2_item` 的 file 特例同样保留。
* `MessageSequence.ToText(TextOptions)` 对应 `SessionMessage.process`：重活（图片/表情描述、
  语音转写、回复内容查库）通过 `TextOptions` 的注入钩子完成，保证本包零 I/O 依赖；
  钩子为空时使用 Python 的兜底文案（如 `[回复了一条消息，但原消息已无法访问]`）。
* `MessageRecord` / `ToDatabaseRecord` / `FromDatabaseRecord` 是 `mai_messages` 行的纯结构体
  表示，`internal/database`（P1）可以直接绑定而不产生包循环；`raw_content` 是 JSON。
  空群名**不会**把群消息降级成私聊（对齐 Python 注释）。

## 验收

`go test ./internal/message/...`（18 个测试）覆盖：session_id 向量、路由元数据优先级、
JSON 往返（含 forward 嵌套与未知类型降级）、占位符、组件渲染、语音占位文本清理、
message id 分配、DB 记录往返、`SessionMessage.Process`。
