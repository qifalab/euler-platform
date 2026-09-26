import { createApp } from "vue";
import "@eu/tokens/style.css";
import "./cloud/cloud.css";
import App from "./App.vue";
import { router } from "./router";

// Application cloud uses an independent cookie session and project model.
// Legacy IaaS/Wujie sources remain for migration, not default bootstrap.
createApp(App).use(router).mount("#app");
