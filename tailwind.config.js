// Tailwind CSS build configuration (v3, matches the classes used in frontend/).
// Rebuild the stylesheet after changing classes in any frontend file:
//   npx tailwindcss@3.4.17 -c tailwind.config.js -i frontend/tailwind.input.css -o frontend/tailwind.css --minify
// The Docker build runs the same command, so images always ship a fresh build.
module.exports = {
  darkMode: 'class',
  content: ['./frontend/**/*.{html,js}'],
};
