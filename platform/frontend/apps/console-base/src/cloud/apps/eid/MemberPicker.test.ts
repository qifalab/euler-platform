import { describe, expect, it, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { defineComponent, ref } from "vue";
import MemberPicker from "./MemberPicker.vue";
import { appContextKey, type AppContext } from "../shared";

describe("EID query authorization member picker", () => {
  it("keeps authorization selections while paging and searching beyond the first 100 members", async () => {
    const request = vi.fn(async (path: string) => {
      const query = new URL(path, "http://test.local").searchParams;
      const page = Number(query.get("page"));
      const searching = query.get("search") === "Member 101";
      return {
        items: searching
          ? [{ actorId: "u101", name: "Member 101" }]
          : Array.from({ length: 25 }, (_, i) => ({
              actorId: `u${(page - 1) * 25 + i + 1}`,
              name: `Member ${(page - 1) * 25 + i + 1}`,
            })),
        total: searching ? 1 : 125,
        page,
      };
    });
    const Host = defineComponent({
      components: { MemberPicker },
      setup() {
        return { selected: ref<string[]>([]) };
      },
      template:
        '<MemberPicker v-model="selected"/><output>{{selected.join(",")}}</output>',
    });
    const wrapper = mount(Host, {
      global: {
        provide: {
          [appContextKey as symbol]: { request } as unknown as AppContext,
        },
      },
    });
    await flushPromises();
    const click = async (text: string) => {
      const button = wrapper.findAll("button").find((b) => b.text() === text);
      if (!button) throw new Error(text);
      await button.trigger("click");
      await flushPromises();
    };
    await wrapper.find('input[value="u1"]').setValue(true);
    await click("下一页会员");
    await wrapper.find('input[value="u26"]').setValue(true);
    await wrapper.find('[aria-label="搜索授权会员"]').setValue("Member 101");
    await click("搜索会员");
    await wrapper.find('input[value="u101"]').setValue(true);
    expect(wrapper.find("output").text()).toBe("u1,u26,u101");
    expect(request).toHaveBeenCalledWith(
      "/admin/members?page=1&pageSize=25&search=Member%20101",
    );
    await wrapper.find('[aria-label="移除 Member 26"]').trigger("click");
    expect(wrapper.find("output").text()).toBe("u1,u101");
    expect(wrapper.text()).toContain("已选 2 / 100");
    wrapper.unmount();
  });
});
