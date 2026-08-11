/**
 * console-ecs — minimal compute sub-app root component (source-only scaffold).
 *
 * Stands in for the `console-compute` category sub-app. In the full architecture
 * this would be a Vue SFC rendered inside the Wujie sandbox; here it is kept as
 * a plain module so it runs without a build step.
 */
export function App({ props = {} } = {}) {
  const { regionId = "cn-north-1", token } = props;
  const el = document.createElement("section");
  el.className = "ecs-app";

  const header = document.createElement("h2");
  header.textContent = "云服务器 ECS (SCECS)";

  const region = document.createElement("p");
  region.textContent = `当前地域: ${regionId}${token ? " · 已注入会话" : ""}`;

  const list = document.createElement("ul");
  const items = ["实例列表", "磁盘", "镜像", "安全组", "购买向导"];
  items.forEach((t) => {
    const li = document.createElement("li");
    li.textContent = t;
    list.appendChild(li);
  });

  el.append(header, region, list);
  return el;
}
