// 右键菜单(popover 顶层渲染,不被抽屉 transform/滚动容器裁剪)
import { reactive } from "vue";

export const ctxState = reactive({ items: [], visible: false });

// items: [{label, action, disabled?} | "-"]
export function showCtxMenu(x, y, items) {
  ctxState.items = items;
  ctxState.visible = true;
  ctxState.x = x;
  ctxState.y = y;
}

export function hideCtxMenu() {
  ctxState.visible = false;
}
