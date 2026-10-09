package emoji

import (
	"fmt"
	"strings"
)

// fallbackText 返回 prompts/zh-CN 对应 .prompt 的内联副本（未注入
// PromptRenderer 时的回退；文案照抄提示词文件）。
func fallbackText(name string, vars map[string]string, _ string) string {
	var value string
	switch name {
	case "emoji_content_analysis":
		imageType := vars["image_type"]
		if imageType == "" {
			imageType = "STATIC"
		}
		value = fmt.Sprintf("输入类型：%s\n"+
			"这是一个表情包图片。输入类型为 GIF 时，图片由动态图中抽取的不同帧横向拼接而成；输入类型为 STATIC 时，图片为单张静态图。\n"+
			"请提取这个表情包主要表达的情绪、语气或常见使用场景标签，最多 5 个，使用逗号分隔。只返回纯文本标签列表，不要解释或输出其他内容。", imageType)
	case "emoji_content_filtration":
		value = "这是一个表情包，请对这个表情包进行审核，标准如下：\n" +
			"1. 必须符合\"符合公序良俗\"的要求\n" +
			"2. 不能是色情、暴力、等违法违规内容，必须符合公序良俗\n" +
			"3. 不能是任何形式的截图，聊天记录或视频截图\n" +
			"4. 不要出现5个以上文字\n" +
			"请回答这个表情包是否满足上述要求，是则回答是，否则回答否，不要出现任何其他内容"
	case "emoji_selection":
		emojiCount := vars["emoji_count"]
		gridRows := vars["grid_rows"]
		gridColumns := vars["grid_columns"]
		value = "你需要根据上下文和当前语气,选择一个合适的表情包来发送\n" +
			"其中包含一张 " + gridRows + "x" + gridColumns + " 的表情包拼图，一共 " + emojiCount + " 个位置。\n" +
			"每张小图左上角都有一个较大的序号，范围是 1 到 " + emojiCount + "。\n" +
			"你需要从这 " + emojiCount + " 张图里选出最合适的一张表情包。\n" +
			"你必须返回一个 JSON 对象（json object），不要输出任何 JSON 之外的内容。\n" +
			"返回格式为：{{\"emoji_index\":1,\"reason\":\"简短理由\"}}"
	case "emoji_replace":
		value = strings.NewReplacer(
			"{nickname}", vars["nickname"],
			"{emoji_num}", vars["emoji_num"],
			"{emoji_num_max}", vars["emoji_num_max"],
			"{emoji_list}", vars["emoji_list"],
			"{description}", vars["description"],
		).Replace("{nickname}的可发送表情包数量已满({emoji_num}/{emoji_num_max})，需要决定是否取消注册一个旧表情包来为新表情包腾出可发送名额。\n\n" +
			"新表情包信息：\n描述: {description}\n\n现有表情包列表：\n{emoji_list}\n\n请决定：\n" +
			"1. 是否要取消注册某个现有表情包来为新表情包腾出名额？\n" +
			"2. 如果要取消注册，应该取消注册哪一个(给出编号)？\n" +
			"请只回答：'不取消注册'或'取消注册编号X'(X为表情包编号)。")
	default:
		return ""
	}
	return value
}
