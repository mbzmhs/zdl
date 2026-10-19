import { createApp } from "vue";
import App from "./App.vue";
import { boot } from "./store.js";
import "./style.css";

createApp(App).mount("#app");
boot();
