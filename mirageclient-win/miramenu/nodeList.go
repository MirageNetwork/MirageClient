package miramenu

import "github.com/tailscale/walk"

type peerNodeListMenu walk.Action // 能归在一个用户/tag下的节点列表
/*
	listMenu   *walk.Action     // 列表外部菜单项
	listTitle  *walk.Action     // 列表头（展示用户账号）
	listSepara *walk.Action     // 列表头分隔
	list       *walk.ActionList //节点列表
*/
type peerNodeListMenuList walk.ActionList

func (al *peerNodeListMenuList) Len() int {
	return (*walk.ActionList)(al).Len()

}
