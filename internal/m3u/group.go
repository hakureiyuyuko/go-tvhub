// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 hakureiyuyuko

package m3u

import "strings"

// groupRule 是一组「命中关键字即归入该分组」的规则，按顺序匹配，先命中者优先。
type groupRule struct {
	Group string
	Keys  []string
}

// 仅在源文件没有 group-title 时使用，用于把上百个频道归拢成可浏览的分组。
var defaultGroupRules = []groupRule{
	{"央视", []string{"cctv", "央视", "cgtn", "中央新影"}},
	{"IPTV专区", []string{"iptv"}},
	{"卫视", []string{"卫视"}},
	{"重庆", []string{"重庆"}},
	{"上海", []string{"上海"}},
	{"影视剧场", []string{"剧场", "影院", "电影", "影视", "影迷", "佳片", "曲艺"}},
	{"体育", []string{"体育", "足球", "台球", "网球", "高尔夫", "垂钓", "武术", "赛事", "球"}},
	{"少儿教育", []string{"少儿", "卡通", "动漫", "动画", "早教", "教育", "学生", "学堂", "宝贝", "启蒙"}},
	{"纪录科教", []string{"纪录", "纪实", "地理", "科教", "解密", "文化", "精品", "国学", "探索", "发现", "求索"}},
	{"音乐戏曲", []string{"音乐", "戏曲", "相声", "小品", "文艺", "娱乐", "时尚", "美妆", "美人", "欢乐"}},
	{"生活服务", []string{"生活", "交通", "农业", "农村", "汽摩", "红岩", "红叶", "购物", "健康", "法治", "社会"}},
}

// GuessGroup 依据频道名推断分组，无法判断时归入「其他」。
func GuessGroup(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return "其他"
	}
	for _, r := range defaultGroupRules {
		for _, k := range r.Keys {
			if strings.Contains(n, strings.ToLower(k)) {
				return r.Group
			}
		}
	}
	return "其他"
}
